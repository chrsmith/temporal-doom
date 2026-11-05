// Package netplay runs multiplayer games in the web server.
//
// In lockstep, nobody sees a tic until every player's input for it has
// reached the arbiter and come back, so input lag is a few round trips to
// the arbiter. If the arbiter is the workflow, each of those trips goes
// browser → server → Temporal → worker and back, about 175ms. So instead,
// the web server is the arbiter. It's the closest point every player can
// reach, about 25ms away.
//
// Each game gets a room. Browsers connect to it over a WebSocket and stream
// their ticcmds; the room merges them into tics (see sequencer) and sends
// each one to every browser the moment it's complete. About once a second
// it records what it has played in the workflow with a "record" update.
// Nothing waits for that: the workflow is the recorder and referee, not
// part of the real-time loop. Its checksums let clients prove they're in
// sync, and if the server dies, the game picks up from the last record.
//
// A game's room lives in one server process, so this only works with a
// single server. (Routing every player of a game to the same server would
// lift that.)
package netplay

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	temporalclient "go.temporal.io/sdk/client"

	"github.com/temporal-community/temporal-doom/internal/game"
)

// Backend is how rooms reach the workflow.
type Backend interface {
	// Net returns the workflow's roster and tic (QueryNet).
	Net(ctx context.Context, id string) (game.NetInfo, error)
	// Sync returns what a client needs to rebuild the game (QuerySync).
	Sync(ctx context.Context, id string) (game.Sync, error)
	// Record sends the "record" update.
	Record(ctx context.Context, id string, rec game.NetRecord) (game.TicResult, error)
}

// Temporal is the Backend for a real Temporal client.
type Temporal struct{ Client temporalclient.Client }

func (t Temporal) Net(ctx context.Context, id string) (game.NetInfo, error) {
	var info game.NetInfo
	return info, t.query(ctx, id, game.QueryNet, &info)
}

func (t Temporal) Sync(ctx context.Context, id string) (game.Sync, error) {
	var s game.Sync
	return s, t.query(ctx, id, game.QuerySync, &s)
}

func (t Temporal) Record(ctx context.Context, id string, rec game.NetRecord) (game.TicResult, error) {
	var res game.TicResult
	return res, game.SendUpdate(ctx, t.Client, id, game.UpdateRecord, rec, &res)
}

func (t Temporal) query(ctx context.Context, id, name string, out any) error {
	v, err := t.Client.QueryWorkflow(ctx, id, "", name)
	if err != nil {
		return err
	}
	return v.Get(out)
}

// Hub holds the server's rooms, one per multiplayer game that someone is
// connected to.
type Hub struct {
	backend    Backend
	ctx        context.Context
	flushEvery time.Duration

	mu    sync.Mutex
	rooms map[string]*room
	stats RecordStats
}

// RecordStats summarizes the record updates sent since the last call to
// Hub.Stats.
type RecordStats struct {
	Records, Errors int
	Total, Max      time.Duration
}

func NewHub(ctx context.Context, b Backend) *Hub {
	return &Hub{backend: b, ctx: ctx, flushEvery: FlushEvery, rooms: map[string]*room{}}
}

// Serve connects a browser to a game's room over a WebSocket. The player
// query parameter is the client's player ID.
func (h *Hub) Serve(w http.ResponseWriter, req *http.Request, id string) {
	player := req.URL.Query().Get("player")
	if player == "" || len(player) > 64 {
		http.Error(w, "need a player ID of up to 64 characters", http.StatusBadRequest)
		return
	}
	r := h.acquire(id)
	defer h.release(r)
	<-r.ready
	if r.err != nil {
		// Don't pass Temporal's error on: it could describe the server's
		// internals.
		if isOver(r.err) {
			http.Error(w, "game is over", http.StatusConflict)
		} else {
			log.Printf("%q: can't open room: %v", id, r.err)
			http.Error(w, "no such multiplayer game", http.StatusNotFound)
		}
		return
	}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return // Upgrade has already replied
	}
	c := newClient(conn, player)
	go c.writeLoop()
	if !r.do(func() { r.attach(c) }) {
		conn.Close()
		return
	}
	c.readLoop(r)
	r.do(func() { r.detach(c) })
}

// End closes a game's room, if it has one, because the game is over.
func (h *Hub) End(id, reason string) {
	h.mu.Lock()
	r := h.rooms[id]
	h.mu.Unlock()
	if r != nil {
		r.do(func() { r.end(reason) })
	}
}

// Busy reports whether other people are playing a game (in it, and not
// away) and player isn't one of them. Games without a room have nobody
// connected, so aren't.
func (h *Hub) Busy(id, player string) bool {
	return h.ask(id, func(r *room) bool {
		others := false
		for _, s := range r.seq.slots {
			if s.playing() {
				if s.ID == player {
					return false
				}
				others = true
			}
		}
		return others
	})
}

// Full reports whether a game has no slot for player: they don't have one
// already, and the others are all taken. Games without a room aren't full
// as far as this knows; joining one finds out.
func (h *Hub) Full(id, player string) bool {
	return h.ask(id, func(r *room) bool {
		return r.seq.slotOf(player) < 0 && r.seq.freeSlot() < 0
	})
}

// ask runs f on a game's room, if it has an open one, and returns the
// result. Otherwise it returns false.
func (h *Hub) ask(id string, f func(*room) bool) bool {
	h.mu.Lock()
	r := h.rooms[id]
	h.mu.Unlock()
	if r == nil {
		return false
	}
	res := make(chan bool, 1)
	if !r.do(func() { res <- !r.stopped && r.seq != nil && f(r) }) {
		return false
	}
	select {
	case v := <-res:
		return v
	case <-r.done: // closed before it got to f
		return false
	}
}

// Stats returns and resets the record update statistics.
func (h *Hub) Stats() (rooms int, s RecordStats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, h.stats = h.stats, RecordStats{}
	return len(h.rooms), s
}

// LogStats logs record update latency every interval, until ctx is done.
// This is the one Temporal call left in multiplayer, and nothing waits for
// it, but it's still worth keeping an eye on.
func (h *Hub) LogStats(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rooms, s := h.Stats()
		if s.Records == 0 && s.Errors == 0 {
			continue
		}
		log.Printf("netplay: %d rooms, %d records (%d failed), avg %v, max %v",
			rooms, s.Records, s.Errors, (s.Total / time.Duration(max(s.Records, 1))).Round(time.Millisecond), s.Max.Round(time.Millisecond))
	}
}

func (h *Hub) observe(d time.Duration, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.stats.Errors++
		return
	}
	h.stats.Records++
	h.stats.Total += d
	h.stats.Max = max(h.stats.Max, d)
}

// acquire returns a game's room, opening it if need be.
func (h *Hub) acquire(id string) *room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		r = &room{
			id:    id,
			hub:   h,
			ctx:   h.ctx,
			ready: make(chan struct{}),
			in:    make(chan func(), 256),
			done:  make(chan struct{}),
		}
		h.rooms[id] = r
		go r.run()
	}
	r.refs++
	return r
}

func (h *Hub) release(r *room) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.refs--
}

// closeIfUnused removes a room nobody is using, and reports whether it did.
func (h *Hub) closeIfUnused(r *room) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.refs > 0 {
		return false
	}
	h.forgetLocked(r)
	return true
}

// forget removes a room, so the next connection to its game opens a new one.
func (h *Hub) forget(r *room) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgetLocked(r)
}

func (h *Hub) forgetLocked(r *room) {
	if h.rooms[r.id] == r {
		delete(h.rooms, r.id)
	}
}

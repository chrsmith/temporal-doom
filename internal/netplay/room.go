package netplay

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/temporal-community/temporal-doom/internal/doomvm"
	"github.com/temporal-community/temporal-doom/internal/game"
)

const (
	// FlushEvery is how often a room records what it has played in the
	// workflow. Nothing waits for it, so this only decides how much of the
	// game is lost if the server dies, and how far behind the workflow's
	// frame query is.
	FlushEvery = time.Second
	// tickEvery is how often a room checks its timers.
	tickEvery = 100 * time.Millisecond
	// closeAfter is how long a room stays open with nobody connected.
	closeAfter = 30 * time.Second
	// recordTimeout bounds a record update, which waits for the worker.
	recordTimeout = 30 * time.Second
	// maxFailures is how many records in a row the workflow can turn down
	// before the room gives up on what it hasn't recorded, and starts over
	// from the workflow's game.
	maxFailures = 5
)

// A room runs one multiplayer game in the server. All of its state belongs
// to one goroutine (run), and everything else talks to it through do.
type room struct {
	id  string
	hub *Hub
	ctx context.Context

	ready chan struct{} // closed once the room has loaded the game
	err   error         // why it couldn't, if it couldn't
	in    chan func()
	done  chan struct{}
	refs  int // connections using the room; guarded by hub.mu

	seq     *sequencer
	clients map[*client]bool
	gen     int // bumped when the room starts over from the workflow

	// log holds the tics that have been played but not recorded yet,
	// starting at tic logStart, when the players in logInGame were in the
	// game. A record in flight covers the first `flushing` of them.
	log       []doomvm.NetTic
	logStart  int32
	logInGame uint8
	flushing  int
	lastFlush time.Time
	failures  int

	lastRoster  int
	lastWaiting uint8
	idleSince   time.Time
	stopped     bool
}

// do runs f on the room's goroutine. It reports false if the room has
// closed.
func (r *room) do(f func()) bool {
	select {
	case r.in <- f:
		return true
	case <-r.done:
		return false
	}
}

func (r *room) run() {
	defer close(r.done)
	info, err := r.hub.backend.Net(r.ctx, r.id)
	if err == nil && info.Over {
		err = errOver
	}
	if err != nil {
		r.err = err
		close(r.ready)
		r.hub.forget(r)
		return
	}
	r.clients = map[*client]bool{}
	r.restart(info)
	close(r.ready)
	log.Printf("%s: room open at tic %d", r.id, info.Tic)

	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for !r.stopped {
		select {
		case f := <-r.in:
			f()
		case now := <-t.C:
			r.tick(now)
		}
		r.update(time.Now())
	}
}

// restart (re)loads the game from the workflow. Anything played since the
// last record is gone, so every client resyncs.
func (r *room) restart(info game.NetInfo) {
	now := time.Now()
	r.gen++
	r.seq = newSequencer(info, now)
	r.log, r.logStart, r.logInGame = nil, info.Tic, inGameOf(info.Slots)
	r.flushing, r.failures = 0, 0
	r.lastRoster = -1
	for c := range r.clients {
		// Whoever was playing carries on.
		if c.playing {
			if i, err := r.seq.addPlayer(c.player, c.name, now); err == nil {
				c.send(seatMsg{T: "seat", Slot: i, Playing: true, Tic: r.seq.tic})
			}
		}
		r.startSync(c)
	}
}

// update runs whatever tics are ready, and tells clients what changed.
// It runs after everything that happens in the room.
func (r *room) update(now time.Time) {
	if r.stopped {
		return
	}
	if tics := r.seq.advance(now); len(tics) > 0 {
		r.log = append(r.log, tics...)
		r.broadcast(ticsMsg{T: "tics", From: r.seq.tic - int32(len(tics)), Tics: tics})
	}
	for c := range r.clients {
		if !c.synced {
			continue
		}
		slot := r.seq.slotOf(c.player)
		playing := slot >= 0 && r.seq.slots[slot].playing()
		if slot != c.slot || playing != c.playing {
			c.slot, c.playing = slot, playing
			c.send(seatMsg{T: "seat", Slot: slot, Playing: playing, Tic: r.seq.tic})
		}
	}
	if w := r.seq.waiting(now, StallAfter); r.seq.roster != r.lastRoster || w != r.lastWaiting {
		r.lastRoster, r.lastWaiting = r.seq.roster, w
		r.broadcast(r.rosterMsg(now))
	}
}

func (r *room) rosterMsg(now time.Time) rosterMsg {
	return rosterMsg{T: "roster", Players: r.seq.players(), Waiting: r.seq.waiting(now, StallAfter)}
}

func (r *room) tick(now time.Time) {
	r.seq.housekeeping(now)
	r.maybeFlush(now)

	if len(r.clients) > 0 || len(r.log) > 0 || r.flushing > 0 {
		r.idleSince = time.Time{}
		return
	}
	if r.idleSince.IsZero() {
		r.idleSince = now
	}
	if now.Sub(r.idleSince) > closeAfter && r.hub.closeIfUnused(r) {
		log.Printf("%s: room closed at tic %d", r.id, r.seq.tic)
		r.stopped = true
	}
}

// broadcast sends a message to every client that is following the game.
func (r *room) broadcast(m any) {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	for c := range r.clients {
		if c.synced {
			c.sendRaw(b)
		}
	}
}

// ---------------------------------------------------------------- clients

func (r *room) attach(c *client) {
	for old := range r.clients {
		if old.player == c.player {
			// The same player in another tab (a duplicated tab copies
			// sessionStorage): the newest one wins.
			old.send(errorMsg{T: "error", Error: "this player connected from somewhere else"})
			r.drop(old)
			// Until the new connection joins, the game shouldn't wait
			// for the old one's input.
			if i := r.seq.slotOf(c.player); i >= 0 {
				r.seq.setAway(i, time.Now())
			}
		}
	}
	r.clients[c] = true
	r.startSync(c)
}

// detach is called when a client's connection closes. A player who
// disconnects is away until they come back.
func (r *room) detach(c *client) {
	if !r.clients[c] {
		return
	}
	r.drop(c)
	if i := r.seq.slotOf(c.player); i >= 0 {
		r.seq.setAway(i, time.Now())
	}
}

// drop disconnects a client.
func (r *room) drop(c *client) {
	delete(r.clients, c)
	c.close()
}

func (r *room) onMessage(c *client, m clientMsg) {
	if !r.clients[c] {
		return
	}
	now := time.Now()
	i := r.seq.slotOf(c.player)
	switch m.T {
	case "join":
		c.name = game.PlayerName(m.Name)
		slot, err := r.seq.addPlayer(c.player, c.name, now)
		if err != nil {
			m := errorMsg{T: "error", Error: err.Error()}
			if errors.Is(err, errFull) {
				m.Code = "full"
			}
			c.send(m)
			return
		}
		// Always answer a join, even if nothing changed: the client
		// counts its input from Tic.
		c.slot, c.playing = slot, true
		c.send(seatMsg{T: "seat", Slot: slot, Playing: true, Tic: r.seq.tic})
	case "cmds":
		if i < 0 || !r.seq.slots[i].playing() {
			return // the client has been (or is about to be) told it's away
		}
		if err := r.seq.input(i, m.Cmds, now); err != nil {
			c.send(errorMsg{T: "error", Error: err.Error()})
			r.detach(c)
		}
	case "pause":
		if i >= 0 {
			r.seq.setAway(i, now)
		}
	case "leave":
		if i >= 0 {
			r.seq.removePlayer(i, now)
		}
	case "resync":
		r.startSync(c)
	case "ping":
		c.send(pongMsg{T: "pong", TS: m.TS})
	}
}

// startSync sends a client everything it needs to run the game from
// scratch: the workflow's snapshot and tic log (the sync query), plus the
// tics the room has played since. After that it follows the game live.
//
// The query runs on its own goroutine, so the game doesn't stall for it.
func (r *room) startSync(c *client) {
	c.synced = false
	c.syncs++
	n, gen := c.syncs, r.gen
	go func() {
		s, err := r.hub.backend.Sync(r.ctx, r.id)
		r.do(func() { r.synced(c, n, gen, s, err) })
	}()
}

func (r *room) synced(c *client, n, gen int, s game.Sync, err error) {
	if !r.clients[c] || n != c.syncs {
		return // gone, or superseded by a newer sync
	}
	retry := func() {
		time.AfterFunc(250*time.Millisecond, func() {
			r.do(func() {
				if r.clients[c] && n == c.syncs {
					r.startSync(c)
				}
			})
		})
	}
	switch {
	case err != nil:
		log.Printf("%s: sync for %s: %v", r.id, c.player, err)
		retry()
		return
	case gen != r.gen || s.Tic < r.logStart:
		// The workflow recorded more (or started over) while the query
		// was in flight.
		r.startSync(c)
		return
	case s.Tic > r.seq.tic:
		log.Printf("%s: workflow is at tic %d, ahead of the room at %d", r.id, s.Tic, r.seq.tic)
		retry()
		return
	}
	c.send(syncMsg{T: "sync", Sync: s, Pending: r.log[s.Tic-r.logStart:]})
	c.synced = true
	c.slot = r.seq.slotOf(c.player)
	c.playing = c.slot >= 0 && r.seq.slots[c.slot].playing()
	c.send(seatMsg{T: "seat", Slot: c.slot, Playing: c.playing, Tic: r.seq.tic})
	c.send(r.rosterMsg(time.Now()))
}

// ---------------------------------------------------------------- recording

// maybeFlush records what has been played since the last record, about
// once a second. One record is in flight at a time.
func (r *room) maybeFlush(now time.Time) {
	if r.flushing > 0 || len(r.log) == 0 {
		return
	}
	if now.Sub(r.lastFlush) < r.hub.flushEvery && len(r.log) < game.MaxRecord {
		return
	}
	n := min(len(r.log), game.MaxRecord)
	inGame := applyMembership(r.logInGame, r.log[:n])
	rec := game.NetRecord{StartTic: r.logStart, Tics: slices.Clone(r.log[:n]), Slots: r.seq.rosterFor(inGame)}
	r.flushing, r.lastFlush = n, now
	gen := r.gen
	go func() {
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(r.ctx, recordTimeout)
		res, err := r.hub.backend.Record(ctx, r.id, rec)
		cancel()
		r.hub.observe(time.Since(t0), err)
		r.do(func() { r.recorded(gen, rec, inGame, res, err) })
	}()
}

func (r *room) recorded(gen int, rec game.NetRecord, inGame uint8, res game.TicResult, err error) {
	if gen != r.gen {
		return
	}
	if err == nil {
		r.flushing = 0
		r.commit(len(rec.Tics), inGame)
		// The workflow's engine got the same result as every client's, or
		// a client will find out that it didn't.
		r.broadcast(checkMsg{T: "check", Segment: res.Segment, Tic: res.Stats.Tic, Checksum: res.Stats.Checksum})
		return
	}
	if isOver(err) {
		r.end("game over")
		return
	}
	log.Printf("%s: record at tic %d: %v", r.id, rec.StartTic, err)
	// The update may have been applied even though we didn't hear back, so
	// ask the workflow where it is. Until then, nothing else is recorded.
	go func() {
		info, err := r.hub.backend.Net(r.ctx, r.id)
		r.do(func() { r.reconcile(gen, rec, inGame, info, err) })
	}()
}

func (r *room) reconcile(gen int, rec game.NetRecord, inGame uint8, info game.NetInfo, err error) {
	if gen != r.gen {
		return
	}
	r.flushing = 0
	switch end := rec.StartTic + int32(len(rec.Tics)); {
	case err != nil:
		log.Printf("%s: %v", r.id, err)
	case info.Over:
		r.end("game over")
	case info.Tic == end:
		r.commit(len(rec.Tics), inGame)
	case info.Tic == rec.StartTic && r.failures+1 < maxFailures:
		r.failures++ // try again at the next flush
	default:
		// The workflow's game isn't the one the room has been playing (it
		// was Reset, say), or it won't take our tics. The workflow is the
		// source of truth, so start over from it.
		log.Printf("%s: starting over from the workflow at tic %d (room was at %d)", r.id, info.Tic, r.seq.tic)
		r.restart(info)
	}
}

func (r *room) commit(n int, inGame uint8) {
	r.log = r.log[n:]
	r.logStart += int32(n)
	r.logInGame = inGame
	r.failures = 0
}

// end closes the room for good, because the game is over.
func (r *room) end(reason string) {
	log.Printf("%s: %s", r.id, reason)
	r.broadcast(overMsg{T: "over", Reason: reason})
	for c := range r.clients {
		r.drop(c)
	}
	r.hub.forget(r)
	r.stopped = true
}

var errOver = &overError{}

type overError struct{}

func (*overError) Error() string { return "game is over" }

func isOver(err error) bool {
	return err != nil && strings.Contains(err.Error(), "game is over")
}

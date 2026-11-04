package netplay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
	"github.com/chrsmith/temporal-doom/internal/game"
)

// fakeWorkflow stands in for the workflow: it keeps the recorded tics and
// a fake checksum.
type fakeWorkflow struct {
	mu      sync.Mutex
	tic     int32
	slots   [doomvm.MaxPlayers]game.Slot
	log     []doomvm.NetTic
	records int
	over    bool
	// loseReply, if set, makes the next record succeed but return an
	// error, as if the response had been lost.
	loseReply bool
}

func newFakeWorkflow() *fakeWorkflow {
	return &fakeWorkflow{slots: [4]game.Slot{{InGame: true, Away: true}}}
}

func checksum(tic int32) uint32 { return uint32(tic) * 2654435761 }

func (f *fakeWorkflow) Net(ctx context.Context, id string) (game.NetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return game.NetInfo{Over: f.over, Tic: f.tic, Slots: f.slots}, nil
}

func (f *fakeWorkflow) Sync(ctx context.Context, id string) (game.Sync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return game.Sync{Config: game.GameConfig{Mode: doomvm.Coop}, Tic: f.tic, Checksum: checksum(f.tic), Net: append([]doomvm.NetTic{}, f.log...)}, nil
}

func (f *fakeWorkflow) Record(ctx context.Context, id string, rec game.NetRecord) (game.TicResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.over {
		return game.TicResult{}, errors.New("game is over")
	}
	if rec.StartTic != f.tic {
		return game.TicResult{}, fmt.Errorf("out of sync: record starts at tic %d but the game is at tic %d", rec.StartTic, f.tic)
	}
	f.log = append(f.log, rec.Tics...)
	f.tic += int32(len(rec.Tics))
	f.slots = rec.Slots
	f.records++
	if f.loseReply {
		f.loseReply = false
		return game.TicResult{}, errors.New("context deadline exceeded")
	}
	return game.TicResult{Stats: doomvm.Stats{Tic: f.tic, Checksum: checksum(f.tic)}}, nil
}

func (f *fakeWorkflow) state() (int32, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tic, f.records
}

// testClient is a WebSocket client that keeps track of the game like a
// browser does, without an engine.
type testClient struct {
	t    *testing.T
	conn *websocket.Conn
	msgs chan map[string]json.RawMessage
}

func dial(t *testing.T, url, player string) *testClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(url, "http", "ws", 1)+"/net?player="+player, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &testClient{t: t, conn: conn, msgs: make(chan map[string]json.RawMessage, 1024)}
	go func() {
		defer close(c.msgs)
		for {
			var m map[string]json.RawMessage
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			c.msgs <- m
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return c
}

func (c *testClient) send(m any) {
	if err := c.conn.WriteJSON(m); err != nil {
		c.t.Fatal(err)
	}
}

// next returns the next message of the given type, skipping others.
func (c *testClient) next(typ string, out any) {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("connection closed waiting for %q", typ)
			}
			var got string
			json.Unmarshal(m["t"], &got)
			if got != typ {
				continue
			}
			b, _ := json.Marshal(m)
			if err := json.Unmarshal(b, out); err != nil {
				c.t.Fatal(err)
			}
			return
		case <-timeout:
			c.t.Fatalf("timed out waiting for %q", typ)
		}
	}
}

type syncResult struct {
	game.Sync
	Pending []doomvm.NetTic `json:"pending"`
}

func startRoom(t *testing.T, f *fakeWorkflow, flushEvery time.Duration) (*Hub, string) {
	hub := NewHub(context.Background(), f)
	hub.flushEvery = flushEvery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(w, r, "game")
	}))
	t.Cleanup(srv.Close)
	return hub, srv.URL
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRoom(t *testing.T) {
	f := newFakeWorkflow()
	hub, url := startRoom(t, f, 50*time.Millisecond)

	// Alice connects, syncs, joins and plays alone.
	alice := dial(t, url, "alice")
	var s syncResult
	alice.next("sync", &s)
	if s.Tic != 0 || len(s.Pending) != 0 {
		t.Fatalf("alice's sync: %+v", s)
	}
	alice.send(map[string]any{"t": "join", "name": "alice"})
	var seat seatMsg
	alice.next("seat", &seat)
	for !seat.Playing {
		alice.next("seat", &seat)
	}
	if seat.Slot != 0 || seat.Tic != 0 {
		t.Fatalf("alice's seat: %+v", seat)
	}
	alice.send(map[string]any{"t": "cmds", "cmds": walk(10)})
	var tics ticsMsg
	alice.next("tics", &tics)
	if tics.From != 0 || len(tics.Tics) != 10 {
		t.Fatalf("alice's tics: from %d, %d tics", tics.From, len(tics.Tics))
	}
	// The room records them, and passes the workflow's checksum on.
	var check checkMsg
	alice.next("check", &check)
	if check.Tic != 10 || check.Checksum != checksum(10) {
		t.Fatalf("check %+v", check)
	}

	// Bob joins. His sync is the workflow's log plus whatever the room
	// has played since (here, nothing).
	bob := dial(t, url, "bob")
	bob.next("sync", &s)
	if s.Tic+int32(len(s.Pending)) != 10 {
		t.Fatalf("bob's sync ends at %d+%d", s.Tic, len(s.Pending))
	}
	bob.send(map[string]any{"t": "join", "name": "bob"})
	for bob.next("seat", &seat); !seat.Playing; bob.next("seat", &seat) {
	}
	if seat.Slot != 1 || seat.Tic != 10 {
		t.Fatalf("bob's seat: %+v", seat)
	}
	// Now alice waits for bob, and both get the merged tics.
	alice.send(map[string]any{"t": "cmds", "cmds": walk(5)})
	bob.send(map[string]any{"t": "cmds", "cmds": walk(3)})
	for _, c := range []*testClient{alice, bob} {
		c.next("tics", &tics)
		if tics.From != 10 || len(tics.Tics) != 3 || tics.Tics[0].Join != 1<<1 || len(tics.Tics[1].Cmds) != 2 {
			t.Fatalf("merged tics: %+v", tics)
		}
	}
	waitFor(t, "the room to record tic 13", func() bool { tic, _ := f.state(); return tic == 13 })
	f.mu.Lock()
	if f.slots[1].ID != "bob" || !f.slots[1].InGame {
		t.Fatalf("recorded roster %+v", f.slots)
	}
	f.mu.Unlock()

	// A record whose reply is lost: the room asks the workflow where it
	// is, sees that the record went through, and carries on.
	f.mu.Lock()
	f.loseReply = true
	f.mu.Unlock()
	bob.send(map[string]any{"t": "cmds", "cmds": walk(4)})
	alice.next("tics", &tics)
	if tics.From != 13 || len(tics.Tics) != 2 {
		t.Fatalf("tics %+v", tics)
	}
	waitFor(t, "the lost record", func() bool { _, n := f.state(); return n == 3 })
	alice.send(map[string]any{"t": "cmds", "cmds": walk(2)})
	alice.next("tics", &tics)
	if tics.From != 15 || len(tics.Tics) != 2 {
		t.Fatalf("tics %+v", tics)
	}
	waitFor(t, "the room to record tic 17", func() bool { tic, _ := f.state(); return tic == 17 })

	// Bob disconnects: he's away, and alice plays on.
	bob.conn.Close()
	var roster rosterMsg
	for alice.next("roster", &roster); len(roster.Players) < 2 || roster.Players[1].Status != "away"; alice.next("roster", &roster) {
	}
	alice.send(map[string]any{"t": "cmds", "cmds": walk(3)})
	alice.next("tics", &tics)
	if tics.From != 17 || len(tics.Tics) != 3 || len(tics.Tics[0].Cmds) != 1 {
		t.Fatalf("alice alone again: %+v", tics)
	}
	waitFor(t, "the room to record tic 20", func() bool { tic, _ := f.state(); return tic == 20 })

	// Someone Resets the workflow to an earlier point. The room's next
	// record is turned down, so it starts over from the workflow's game:
	// alice gets a fresh sync and keeps playing.
	f.mu.Lock()
	f.tic, f.log = 10, f.log[:10]
	f.mu.Unlock()
	alice.send(map[string]any{"t": "cmds", "cmds": walk(2)})
	alice.next("sync", &s)
	if s.Tic != 10 || len(s.Pending) != 0 {
		t.Fatalf("sync after reset: tic %d + %d", s.Tic, len(s.Pending))
	}
	alice.next("seat", &seat)
	if !seat.Playing || seat.Tic != 10 {
		t.Fatalf("seat after reset: %+v", seat)
	}
	alice.send(map[string]any{"t": "cmds", "cmds": walk(1)})
	alice.next("tics", &tics)
	if tics.From != 10 {
		t.Fatalf("tics after reset from %d", tics.From)
	}

	// The game ends.
	hub.End("game", "quit")
	var over overMsg
	alice.next("over", &over)
	if over.Reason != "quit" {
		t.Fatalf("over %+v", over)
	}
}

func TestRoomGameOver(t *testing.T) {
	f := newFakeWorkflow()
	f.over = true
	_, url := startRoom(t, f, 50*time.Millisecond)
	_, resp, err := websocket.DefaultDialer.Dial(strings.Replace(url, "http", "ws", 1)+"/net?player=late", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("connecting to a finished game: %v %v", err, resp)
	}
}

func TestRoomSyncIncludesUnrecordedTics(t *testing.T) {
	f := newFakeWorkflow()
	_, url := startRoom(t, f, time.Hour) // nothing gets recorded

	alice := dial(t, url, "alice")
	var s syncResult
	alice.next("sync", &s)
	alice.send(map[string]any{"t": "join"})
	alice.send(map[string]any{"t": "cmds", "cmds": walk(7)})
	var tics ticsMsg
	alice.next("tics", &tics)

	bob := dial(t, url, "bob")
	bob.next("sync", &s)
	if s.Tic != 0 || len(s.Pending) != 7 {
		t.Fatalf("bob's sync: tic %d + %d pending", s.Tic, len(s.Pending))
	}
	if _, records := f.state(); records != 0 {
		t.Fatalf("%d records", records)
	}
}

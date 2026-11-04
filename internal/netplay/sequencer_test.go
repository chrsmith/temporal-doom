package netplay

import (
	"strings"
	"testing"
	"time"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
	"github.com/chrsmith/temporal-doom/internal/game"
)

func walk(n int) []doomvm.TicCmd {
	cmds := make([]doomvm.TicCmd, n)
	for i := range cmds {
		cmds[i] = doomvm.TicCmd{Forward: game.MaxMove}
	}
	return cmds
}

// newGame is a new game as the workflow starts it: slot 0's marine is
// there, waiting for the first player.
func newGame(now time.Time) *sequencer {
	return newSequencer(game.NetInfo{Slots: [4]game.Slot{{InGame: true, Name: "host", Away: true}}}, now)
}

func TestSequencer(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	q := newGame(now)
	mustAdd := func(id string) int {
		t.Helper()
		i, err := q.addPlayer(id, id, now)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	mustInput := func(i int, cmds []doomvm.TicCmd) {
		t.Helper()
		if err := q.input(i, cmds, now); err != nil {
			t.Fatal(err)
		}
	}

	// The first player claims the marine waiting at the start. Alone,
	// their input runs straight away.
	if a := mustAdd("alice"); a != 0 {
		t.Fatalf("alice got slot %d", a)
	}
	mustInput(0, walk(10))
	if tics := q.advance(now); len(tics) != 10 || tics[0].Join != 0 || q.tic != 10 {
		t.Fatalf("alice alone: %d tics, first %+v, now at %d", len(tics), tics[0], q.tic)
	}

	// Now the game waits for bob.
	if b := mustAdd("bob"); b != 1 {
		t.Fatalf("bob got slot %d", b)
	}
	mustInput(0, walk(5))
	if tics := q.advance(now); len(tics) != 0 {
		t.Fatalf("ran %d tics without bob's input", len(tics))
	}
	if w := q.waiting(now, 0); w != 1<<1 {
		t.Fatalf("waiting for %04b, want bob", w)
	}
	if w := q.waiting(now, StallAfter); w != 0 {
		t.Fatalf("reported a stall straight away: %04b", w)
	}
	if w := q.waiting(now.Add(StallAfter), StallAfter); w != 1<<1 {
		t.Fatalf("waiting for %04b after StallAfter, want bob", w)
	}

	// Bob's input arrives. The tics both players have input for run,
	// starting with bob's arrival.
	mustInput(1, walk(3))
	tics := q.advance(now)
	if len(tics) != 3 || tics[0].Join != 1<<1 || tics[1].Join != 0 || q.tic != 13 {
		t.Fatalf("bob arrives: %+v", tics)
	}
	if len(tics[1].Cmds) != 2 || tics[1].Cmds[1].Forward != game.MaxMove {
		t.Fatalf("merged tic doesn't carry both players' input: %+v", tics[1])
	}

	// Bob pauses: alice's queued input runs, and bob's marine stands still.
	q.setAway(1, now)
	if tics := q.advance(now); len(tics) != 2 || len(tics[0].Cmds) != 1 {
		t.Fatalf("after bob paused: %+v", tics)
	}
	if p := q.players(); len(p) != 2 || p[1].Status != "away" {
		t.Fatalf("players %+v", p)
	}

	// Bob comes back, then goes quiet without pausing. After AwayAfter,
	// he's treated as away, and the game goes on.
	mustAdd("bob")
	mustInput(0, walk(5))
	if changed := q.housekeeping(now.Add(AwayAfter / 2)); changed != 0 {
		t.Fatalf("housekeeping changed %04b too early", changed)
	}
	if changed := q.housekeeping(now.Add(AwayAfter + time.Second)); changed != 1<<1 {
		t.Fatalf("housekeeping changed %04b, want bob", changed)
	}
	if tics := q.advance(now); len(tics) != 5 || q.tic != 20 {
		t.Fatalf("after bob went quiet: %d tics, at %d", len(tics), q.tic)
	}

	// A third player arrives, and bob leaves for good: both happen with
	// the next tic.
	if c := mustAdd("carol"); c != 2 {
		t.Fatalf("carol got slot %d", c)
	}
	q.removePlayer(q.slotOf("bob"), now)
	mustInput(0, walk(1))
	mustInput(2, walk(1))
	if tics := q.advance(now); len(tics) != 1 || tics[0].Leave != 1<<1 || tics[0].Join != 1<<2 {
		t.Fatalf("carol in, bob out: %+v", tics)
	}

	// The workflow is told who's in the game after the tics it records.
	// Someone who has joined but hasn't run a tic yet waits for the next
	// record.
	if d := mustAdd("dave"); d != 1 {
		t.Fatalf("dave got slot %d", d)
	}
	r := q.rosterFor(0b101)
	if r[0].ID != "alice" || r[1].InGame || r[2].ID != "carol" || r[3].InGame {
		t.Fatalf("roster %+v", r)
	}

	// Cheats and runaway clients are turned away.
	if err := q.input(0, []doomvm.TicCmd{{Forward: 100}}, now); err == nil || !strings.Contains(err.Error(), "impossible movement") {
		t.Fatalf("cheat: %v", err)
	}
	if err := q.input(0, walk(MaxQueue+1), now); err == nil || !strings.Contains(err.Error(), "too far ahead") {
		t.Fatalf("runaway: %v", err)
	}
	// Eve takes the last slot, and then the game is full.
	if e := mustAdd("eve"); e != 3 {
		t.Fatalf("eve got slot %d", e)
	}
	if _, err := q.addPlayer("frank", "frank", now); err != errFull {
		t.Fatalf("fifth player: %v", err)
	}

	// Away players are removed after RemoveAfter.
	q.setAway(q.slotOf("dave"), now)
	q.housekeeping(now.Add(RemoveAfter + time.Second))
	if q.slotOf("dave") >= 0 {
		t.Fatal("dave is still in the game")
	}
}

func TestSequencerLastPlayerLeaves(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	q := newGame(now)
	q.addPlayer("solo", "solo", now)
	q.input(0, walk(5), now)
	q.advance(now)
	// DOOM can't run with nobody in the game, so the marine stays, open
	// for the next player, and the game doesn't advance without them.
	q.removePlayer(0, now)
	if p := q.players(); len(p) != 1 || p[0].Status != "open" {
		t.Fatalf("players %+v", p)
	}
	if tics := q.advance(now); len(tics) != 0 {
		t.Fatalf("ran %d tics with nobody playing", len(tics))
	}
	if i, _ := q.addPlayer("next", "next", now); i != 0 || q.join != 0 {
		t.Fatalf("next player got slot %d (join %04b)", i, q.join)
	}
}

func TestSequencerFromWorkflow(t *testing.T) {
	// After a server restart, nobody is connected: everyone is away until
	// they reconnect and join.
	q := newSequencer(game.NetInfo{Tic: 700, Slots: [4]game.Slot{
		{ID: "alice", Name: "alice", InGame: true, Deaths: 3},
		{ID: "bob", Name: "bob", InGame: true, Away: true},
	}}, time.Now())
	if q.tic != 700 || q.playing("alice") || q.playing("bob") {
		t.Fatalf("restored %+v", q.slots)
	}
	if i, _ := q.addPlayer("alice", "", time.Now()); i != 0 || q.join != 0 || q.slots[0].Name != "alice" {
		t.Fatalf("alice back in slot %d (join %04b)", i, q.join)
	}
}

package game

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/temporal-community/temporal-doom/internal/doomvm"
)

type anyResult struct {
	rejected, failed error
	value            any
}

type anyCallbacks struct{ r *anyResult }

func (c anyCallbacks) Accept()          {}
func (c anyCallbacks) Reject(err error) { c.r.rejected = err }
func (c anyCallbacks) Complete(v any, err error) {
	c.r.failed = err
	c.r.value = v
}

func (r *anyResult) err() error {
	if r.rejected != nil {
		return r.rejected
	}
	return r.failed
}

// netEnv drives a multiplayer game in the test environment, playing the
// part of the web server.
type netEnv struct {
	t   *testing.T
	env *testsuite.TestWorkflowEnvironment
	wfs *Workflows
	ids int
	now time.Duration
}

func newNetEnv(t *testing.T) *netEnv {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	wfs := &Workflows{Engine: testRuntime(t)}
	env.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: WorkflowName})
	return &netEnv{t: t, env: env, wfs: wfs}
}

// step runs f one simulated second after the previous step. Updates sent
// in one step are delivered after it returns, so steps check the results
// of earlier ones.
func (n *netEnv) step(f func()) {
	n.now += time.Second
	n.env.RegisterDelayedCallback(f, n.now)
}

func (n *netEnv) update(name string, arg any) *anyResult {
	r := &anyResult{}
	n.ids++
	id := fmt.Sprintf("%s-%d", name, n.ids)
	if arg == nil {
		n.env.UpdateWorkflow(name, id, anyCallbacks{r})
	} else {
		n.env.UpdateWorkflow(name, id, anyCallbacks{r}, arg)
	}
	return r
}

func (n *netEnv) query(name string, out any, args ...any) {
	n.t.Helper()
	v, err := n.env.QueryWorkflow(name, args...)
	if err != nil {
		n.t.Fatal(err)
	}
	if err := v.Get(out); err != nil {
		n.t.Fatal(err)
	}
}

// netTics makes n merged tics in which everyone in the game (after the
// first tic's membership changes) walks forward.
func netTics(n int, inGame, join, leave uint8) []doomvm.NetTic {
	inGame = (inGame &^ leave) | join
	tics := make([]doomvm.NetTic, n)
	for k := range tics {
		cmds := make([]doomvm.TicCmd, doomvm.MaxPlayers)
		for p := range cmds {
			if inGame&(1<<p) != 0 {
				cmds[p] = doomvm.TicCmd{Forward: MaxMove}
			}
		}
		tics[k].Cmds = cmds
	}
	tics[0].Join, tics[0].Leave = join, leave
	return tics
}

func player(id string) Slot { return Slot{ID: id, Name: id, InGame: true} }

func TestMultiplayer(t *testing.T) {
	n := newNetEnv(t)
	r := map[string]*anyResult{}
	record := func(key string, start int32, tics []doomvm.NetTic, slots ...Slot) {
		rec := NetRecord{StartTic: start, Tics: tics}
		copy(rec.Slots[:], slots)
		r[key] = n.update(UpdateRecord, rec)
	}
	res := func(key string) TicResult {
		t.Helper()
		if err := r[key].err(); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return r[key].value.(TicResult)
	}
	rejected := func(key, want string) {
		t.Helper()
		if err := r[key].err(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: error %v, want %q", key, err, want)
		}
	}
	state := func() State { var st State; n.query(QueryState, &st); return st }

	// The first player takes over the marine waiting at the start.
	n.step(func() { record("alice", 0, netTics(10, 0b1, 0, 0), player("alice")) })
	n.step(func() {
		if x := res("alice"); x.Stats.Tic != 10 || x.Updates != 1 {
			t.Fatalf("alice alone: %+v", x)
		}
		if st := state(); len(st.Players) != 1 || st.Players[0].Name != "alice" || st.Players[0].Status != "playing" {
			t.Fatalf("players %+v", st.Players)
		}
		record("bob joins", 10, netTics(5, 0b1, 0b10, 0), player("alice"), player("bob"))
	})
	n.step(func() {
		if x := res("bob joins"); x.Stats.Tic != 15 {
			t.Fatalf("bob joins: tic %d", x.Stats.Tic)
		}
		// The workflow referees: none of these get into history.
		record("retried", 10, netTics(5, 0b11, 0, 0), player("alice"), player("bob"))
		cheat := netTics(5, 0b11, 0, 0)
		cheat[2].Cmds[1].Forward = 100
		record("cheat", 15, cheat, player("alice"), player("bob"))
		record("join twice", 15, netTics(5, 0b11, 0b1, 0), player("alice"), player("bob"))
		ghost := netTics(5, 0b11, 0, 0)
		ghost[1].Cmds[3].Buttons = doomvm.ButtonAttack
		record("ghost input", 15, ghost, player("alice"), player("bob"))
		record("bad roster", 15, netTics(5, 0b11, 0, 0), player("alice"), player("bob"), player("carol"))
		record("everyone leaves", 15, netTics(5, 0b11, 0, 0b11))
		record("empty", 15, nil, player("alice"), player("bob"))
	})
	n.step(func() {
		rejected("retried", "out of sync")
		rejected("cheat", "impossible movement")
		rejected("join twice", "already in the game")
		rejected("ghost input", "isn't in the game")
		rejected("bad roster", "roster disagrees")
		rejected("everyone leaves", "nobody is left")
		rejected("empty", "must have 1-")
		// Bob is away (the server plays empty ticcmds for him), and
		// carol takes his slot's neighbor.
		bob := player("bob")
		bob.Away = true
		record("bob away", 15, netTics(5, 0b11, 0b100, 0), player("alice"), bob, player("carol"))
	})
	n.step(func() {
		res("bob away")
		if st := state(); len(st.Players) != 3 || st.Players[1].Status != "away" || st.Players[2].Name != "carol" {
			t.Fatalf("players %+v", st.Players)
		}
		record("bob leaves", 20, netTics(5, 0b111, 0, 0b10), player("alice"), Slot{}, player("carol"))
	})
	n.step(func() {
		if x := res("bob leaves"); x.Stats.Tic != 25 {
			t.Fatalf("bob leaves: tic %d", x.Stats.Tic)
		}
		var info NetInfo
		n.query(QueryNet, &info)
		if info.Over || info.Tic != 25 || info.Slots[0].ID != "alice" || info.Slots[1].InGame || info.Slots[2].ID != "carol" {
			t.Fatalf("net info %+v", info)
		}
		// The client's resync path: a fresh engine, the snapshot and the
		// merged tics end up exactly where the workflow is.
		var sync Sync
		n.query(QuerySync, &sync)
		vm, err := n.wfs.Engine.NewVM(context.Background(), sync.Config.options(sync.Snapshot))
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close()
		vm.SetView(2)
		if err := vm.NetTics(sync.Net); err != nil {
			t.Fatal(err)
		}
		if sum, _ := vm.Checksum(); sum != sync.Checksum || sync.Tic != 25 || len(sync.Net) != 25 {
			t.Fatalf("resynced engine: checksum %08x, want %08x (%d tics)", sum, sync.Checksum, len(sync.Net))
		}
		// The frame query can show any player's view.
		for _, view := range []int{0, 2} {
			var fr Frame
			n.query(QueryFrame, &fr, int32(0), view)
			if len(fr.PNG) == 0 {
				t.Fatalf("no frame for view %d", view)
			}
		}
		// The fifth record reaches MaxUpdatesPerRun.
		record("last", 25, netTics(5, 0b101, 0, 0), player("alice"), Slot{}, player("carol"))
	})

	n.env.ExecuteWorkflow(WorkflowName, GameInput{Config: GameConfig{Player: "alice", Mode: doomvm.Coop, MaxUpdatesPerRun: 5}})

	err := n.env.GetWorkflowError()
	var can *workflow.ContinueAsNewError
	if !asCAN(err, &can) {
		t.Fatalf("workflow error = %v, want continue-as-new", err)
	}
	var next GameInput
	if err := decode(can, &next); err != nil {
		t.Fatal(err)
	}
	if next.Net == nil || next.Net.Slots[0].ID != "alice" || next.Net.Slots[2].ID != "carol" || next.Net.Slots[1].InGame {
		t.Fatalf("continue-as-new carried %+v", next.Net)
	}
	vm, err := n.wfs.Engine.NewVM(context.Background(), next.Config.options(next.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	if p, _ := vm.Players(); p != 0b101 {
		t.Fatalf("snapshot has players %03b, want alice and carol", p)
	}
	if st, _ := vm.Stats(); st.Tic != 30 {
		t.Fatalf("snapshot at tic %d, want 30", st.Tic)
	}
}

func TestMultiplayerQuit(t *testing.T) {
	n := newNetEnv(t)
	var rec *anyResult
	n.step(func() {
		rec = n.update(UpdateRecord, NetRecord{Tics: netTics(5, 0b1, 0, 0), Slots: [4]Slot{player("solo")}})
	})
	n.step(func() { n.update(UpdateQuit, nil) })
	n.env.ExecuteWorkflow(WorkflowName, GameInput{Config: GameConfig{Player: "solo", Mode: doomvm.Deathmatch}})
	if err := n.env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if rec.err() != nil {
		t.Fatal(rec.err())
	}
	// Queries still work on a finished game, so the server needs to be
	// told it's over.
	var info NetInfo
	n.query(QueryNet, &info)
	if !info.Over || info.Tic != 5 {
		t.Fatalf("net info after quit: %+v", info)
	}
}

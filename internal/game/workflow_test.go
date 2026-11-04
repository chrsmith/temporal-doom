package game

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
)

func testRuntime(t *testing.T) *doomvm.Runtime {
	t.Helper()
	wasm, err := os.ReadFile("../../build/doom.wasm")
	if err != nil {
		t.Skip("build/doom.wasm missing; run `make engine`")
	}
	wad, err := os.ReadFile("../../wad/doom1.wad")
	if err != nil {
		t.Skip("wad/doom1.wad missing; run `make wad`")
	}
	rt, err := doomvm.NewRuntime(context.Background(), wasm, wad, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt
}

type updateResult struct {
	rejected, failed error
	result           TicResult
}

type callbacks struct{ r *updateResult }

func (c callbacks) Accept()          {}
func (c callbacks) Reject(err error) { c.r.rejected = err }
func (c callbacks) Complete(v any, err error) {
	c.r.failed = err
	if res, ok := v.(TicResult); ok {
		c.r.result = res
	}
}

func walk(n int) []doomvm.TicCmd {
	cmds := make([]doomvm.TicCmd, n)
	for i := range cmds {
		cmds[i] = doomvm.TicCmd{Forward: MaxMove}
	}
	return cmds
}

func TestWorkflow(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	wfs := &Workflows{Engine: testRuntime(t)}
	env.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: WorkflowName})

	results := map[string]*updateResult{}
	send := func(at time.Duration, id string, b TicBatch) {
		r := &updateResult{}
		results[id] = r
		env.RegisterDelayedCallback(func() {
			env.UpdateWorkflow(UpdateTics, id, callbacks{r}, b)
		}, at)
	}
	send(1*time.Second, "ok1", TicBatch{StartTic: 0, Cmds: walk(35)})
	send(2*time.Second, "stale", TicBatch{StartTic: 0, Cmds: walk(35)})
	send(3*time.Second, "cheat", TicBatch{StartTic: 35, Cmds: []doomvm.TicCmd{{Forward: 100}}})
	send(4*time.Second, "pause", TicBatch{StartTic: 35, Cmds: []doomvm.TicCmd{{Buttons: doomvm.ButtonSpecial}}})
	send(5*time.Second, "ok2", TicBatch{StartTic: 35, Cmds: walk(35)})
	// The third successful update triggers continue-as-new (MaxUpdatesPerRun = 3).
	send(6*time.Second, "ok3", TicBatch{StartTic: 70, Cmds: walk(35)})

	env.ExecuteWorkflow(WorkflowName, GameInput{Config: GameConfig{Player: "test", Skill: 3, MaxUpdatesPerRun: 3}})

	for id, want := range map[string]string{"stale": "out of sync", "cheat": "impossible movement", "pause": "special buttons"} {
		if r := results[id]; r.rejected == nil || !strings.Contains(r.rejected.Error(), want) {
			t.Errorf("%s: rejected = %v, want %q", id, r.rejected, want)
		}
	}
	for _, id := range []string{"ok1", "ok2", "ok3"} {
		if r := results[id]; r.rejected != nil || r.failed != nil {
			t.Errorf("%s: rejected=%v failed=%v", id, r.rejected, r.failed)
		}
	}
	if got := results["ok3"].result.Stats.Tic; got != 105 {
		t.Errorf("tic after ok3 = %d, want 105", got)
	}

	// The run should end by continuing as new, carrying a savegame.
	err := env.GetWorkflowError()
	if !workflow.IsContinueAsNewError(err) {
		t.Fatalf("workflow error = %v, want continue-as-new", err)
	}
	var can *workflow.ContinueAsNewError
	if !asCAN(err, &can) {
		t.Fatalf("not a ContinueAsNewError: %v", err)
	}
	var next GameInput
	if err := decode(can, &next); err != nil {
		t.Fatal(err)
	}
	if next.Segment != 1 || len(next.Snapshot) == 0 || next.EngineVersion != wfs.Engine.Version {
		t.Fatalf("bad continue-as-new input: segment=%d snapshot=%d bytes version=%q", next.Segment, len(next.Snapshot), next.EngineVersion)
	}

	// The snapshot resumes the game where it left off.
	vm, err := wfs.Engine.NewVM(context.Background(), next.Config.options(next.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	st, _ := vm.Stats()
	if st.Tic != 105 || st.Checksum != results["ok3"].result.Stats.Checksum {
		t.Fatalf("snapshot resumed at tic %d checksum %08x, want tic 105 checksum %08x", st.Tic, st.Checksum, results["ok3"].result.Stats.Checksum)
	}
}

func TestIdleTimeout(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	wfs := &Workflows{Engine: testRuntime(t)}
	env.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: WorkflowName})
	env.ExecuteWorkflow(WorkflowName, GameInput{Config: GameConfig{Player: "afk", IdleTimeout: time.Minute}})
	var sum Summary
	if err := env.GetWorkflowResult(&sum); err != nil {
		t.Fatal(err)
	}
	if sum.Reason != "idle" {
		t.Fatalf("reason = %q, want idle", sum.Reason)
	}
}

func TestStateQuerySinglePlayer(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	wfs := &Workflows{Engine: testRuntime(t)}
	env.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: WorkflowName})
	var st State
	env.RegisterDelayedCallback(func() {
		v, err := env.QueryWorkflow(QueryState)
		if err != nil {
			t.Error(err)
		} else if err := v.Get(&st); err != nil {
			t.Error(err)
		}
		env.UpdateWorkflow(UpdateQuit, "quit", callbacks{&updateResult{}})
	}, time.Second)
	env.ExecuteWorkflow(WorkflowName, GameInput{Config: GameConfig{Player: "solo"}})
	if st.Config.Player != "solo" || len(st.Players) != 0 {
		t.Fatalf("state = %+v", st)
	}
}

func asCAN(err error, target **workflow.ContinueAsNewError) bool { return errors.As(err, target) }

func decode(can *workflow.ContinueAsNewError, v any) error {
	return converter.GetDefaultDataConverter().FromPayloads(can.Input, v)
}

func TestSoundsInFrameQuery(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	wfs := &Workflows{Engine: testRuntime(t)}
	env.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: WorkflowName})

	// Pull the trigger at tics 30, 50 and 70 (after the pistol has been
	// raised at the start of the level).
	shoot := make([]doomvm.TicCmd, 90)
	for i := 30; i < len(shoot); i += 20 {
		shoot[i].Buttons = doomvm.ButtonAttack
	}
	r := &updateResult{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateTics, "shoot", callbacks{r}, TicBatch{StartTic: 0, Cmds: shoot})
	}, time.Second)
	var all, later Frame
	env.RegisterDelayedCallback(func() {
		for since, fr := range map[int32]*Frame{0: &all, 60: &later} {
			v, err := env.QueryWorkflow(QueryFrame, since)
			if err != nil {
				t.Error(err)
				return
			}
			if err := v.Get(fr); err != nil {
				t.Error(err)
			}
		}
		env.UpdateWorkflow(UpdateQuit, "quit", callbacks{&updateResult{}})
	}, 2*time.Second)
	env.ExecuteWorkflow(WorkflowName, GameInput{Config: GameConfig{Player: "test"}})

	if r.rejected != nil || r.failed != nil {
		t.Fatalf("update failed: %v %v", r.rejected, r.failed)
	}
	pistol := func(fr Frame) (tics []int32) {
		for _, ev := range fr.Sounds {
			if ev.Kind == "start" && ev.Name == "pistol" {
				tics = append(tics, ev.Tic)
			}
		}
		return tics
	}
	if got := pistol(all); len(got) != 3 {
		t.Fatalf("since 0: pistol shots at tics %v, want 3 (sounds: %+v)", got, all.Sounds)
	}
	if got := pistol(later); len(got) != 1 || got[0] <= 60 {
		t.Fatalf("since 60: pistol shots at tics %v, want just the last one", got)
	}
}

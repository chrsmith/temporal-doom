package doomvm

import (
	"context"
	"os"
	"testing"
)

// TestSnapshotStress chains snapshots the way continue-as-new does: play a
// second, save, boot a fresh engine from the snapshot, and carry on playing
// on the new engine.
//
// Vanilla savegames contain raw pointers from the saving process. gore used
// to turn them back into Go pointers while loading; if the garbage collector
// happened to run mid-load and one landed on a free heap slot, the Go runtime
// inside the engine aborted. Whether that happens depends on heap layout, so
// the only reliable way to catch it is to load a lot of different snapshots.
// Chaining matters: a player object allocated while loading a snapshot sits
// where a fresh engine has a free slot at the time it reads the player.
func TestSnapshotStress(t *testing.T) {
	rt := testRuntime(t)
	if p := os.Getenv("DOOM_WASM"); p != "" {
		wasm, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		wad, _ := os.ReadFile("../../wad/doom1.wad")
		if rt, err = NewRuntime(context.Background(), wasm, wad, ""); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	vm, err := rt.NewVM(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { vm.Close() }()
	loads := 0
	for tic := 0; tic < 35*120; tic += 35 {
		// Like the worker: small batches, draining sounds and rendering a
		// frame after each, so the saving engine's heap looks like the
		// worker's.
		for i := 0; i < 35; i += 5 {
			if err := vm.Tics(script(tic+i, 5)); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.Sounds(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := vm.Render(); err != nil {
				t.Fatal(err)
			}
		}
		if !vm.CanSave() {
			continue
		}
		snap, err := vm.Save()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := vm.Checksum()
		loaded, err := rt.NewVM(ctx, Options{Snapshot: snap})
		if err != nil {
			t.Fatalf("tic %d: %.300v", tic+35, err)
		}
		got, _ := loaded.Checksum()
		if got != want {
			t.Fatalf("tic %d: snapshot loaded with checksum %08x, want %08x", tic+35, got, want)
		}
		vm.Close()
		vm = loaded
		loads++
	}
	if loads < 60 {
		t.Fatalf("only %d snapshots could be taken", loads)
	}
	t.Logf("%d snapshots loaded", loads)
}

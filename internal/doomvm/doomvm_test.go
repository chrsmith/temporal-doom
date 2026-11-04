package doomvm

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

func testRuntime(t testing.TB) *Runtime {
	t.Helper()
	wasm, err := os.ReadFile("../../build/doom.wasm")
	if err != nil {
		t.Skip("build/doom.wasm missing; run `make engine`")
	}
	wad, err := os.ReadFile("../../wad/doom1.wad")
	if err != nil {
		t.Skip("wad/doom1.wad missing; run `make wad`")
	}
	rt, err := NewRuntime(context.Background(), wasm, wad, os.TempDir()+"/temporal-doom-wazero-cache")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt
}

// script is a deterministic but non-trivial stream of inputs: walk, turn,
// shoot and open doors.
func script(start, n int) []TicCmd {
	cmds := make([]TicCmd, n)
	for i := range cmds {
		t := start + i
		c := TicCmd{Forward: 0x32}
		if t%90 < 25 {
			c.AngleTurn = 1280
		}
		if t%50 < 10 {
			c.Side = -0x28
		}
		if t%12 == 0 {
			c.Buttons |= ButtonAttack
		}
		if t%35 == 0 {
			c.Buttons |= ButtonUse
		}
		cmds[i] = c
	}
	return cmds
}

func run(t testing.TB, vm *VM, start, n, batch int) {
	for i := 0; i < n; i += batch {
		if err := vm.Tics(script(start+i, min(batch, n-i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeterminism(t *testing.T) {
	ctx := context.Background()
	rt := testRuntime(t)

	a, err := rt.NewVM(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := rt.NewVM(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Same inputs, different batching, and b renders every batch while a
	// never renders. None of that may affect the simulation.
	run(t, a, 0, 700, 35)
	for i := 0; i < 700; i += 7 {
		if err := b.Tics(script(i, 7)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.Render(); err != nil {
			t.Fatal(err)
		}
	}
	sa, _ := a.Stats()
	sb, _ := b.Stats()
	t.Logf("a: %+v", sa)
	if sa != sb {
		t.Fatalf("desync:\n a=%+v\n b=%+v", sa, sb)
	}
	if sa.Tic != 700 {
		t.Fatalf("tic = %d", sa.Tic)
	}

	// Snapshots: two fresh engines resumed from the same snapshot must
	// stay in lockstep.
	if !a.CanSave() {
		t.Skipf("player can't save (dead?): %+v", sa)
	}
	snap, err := a.Save()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot: %d bytes", len(snap))
	c, err := rt.NewVM(ctx, Options{Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d, err := rt.NewVM(ctx, Options{Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	sc, _ := c.Stats()
	if sc.Tic != 700 || sc.Health != sa.Health {
		t.Fatalf("snapshot didn't restore: %+v", sc)
	}
	run(t, c, 700, 700, 35)
	run(t, d, 700, 700, 10)
	sc, _ = c.Stats()
	sd, _ := d.Stats()
	if sc != sd {
		t.Fatalf("desync after snapshot:\n c=%+v\n d=%+v", sc, sd)
	}
	t.Logf("c: %+v", sc)

	pix, pal, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(pix, []byte{0}) > len(pix)/2 || len(pal) != 768 {
		t.Fatalf("frame looks wrong")
	}
}

func BenchmarkTic(b *testing.B) {
	rt := testRuntime(b)
	vm, err := rt.NewVM(context.Background(), Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vm.Tics(script(i, 1))
	}
}

func BenchmarkRender(b *testing.B) {
	rt := testRuntime(b)
	vm, err := rt.NewVM(context.Background(), Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	vm.Tics(script(0, 10))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vm.Render()
	}
}

func TestStartupTime(t *testing.T) {
	rt := testRuntime(t)
	start := time.Now()
	vm, err := rt.NewVM(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	t.Logf("NewVM took %v", time.Since(start))
}

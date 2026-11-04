package doomvm

import (
	"context"
	"fmt"
	"testing"
)

// netScript is a multiplayer session: players come and go, and everyone in
// the game walks, turns and shoots in their own pattern.
func netScript(start, n int) []NetTic {
	tics := make([]NetTic, n)
	for i := range tics {
		t := start + i
		switch t {
		case 40:
			tics[i].Join = 1<<1 | 1<<3
		case 400:
			tics[i].Leave = 1 << 1
		case 600:
			tics[i].Join = 1 << 2
		case 900:
			tics[i].Leave, tics[i].Join = 1<<3, 1<<3 // the slot changes hands
		case 1200:
			tics[i].Join = 1 << 1
		}
		tics[i].Cmds = make([]TicCmd, MaxPlayers)
		for p := range tics[i].Cmds {
			pc := script(t+p*37, 1)[0]
			if p%2 == 1 {
				pc.AngleTurn = -pc.AngleTurn
			}
			if (t/200+p)%4 == 0 {
				pc = TicCmd{Buttons: ButtonUse} // stand around; respawn if dead
			}
			tics[i].Cmds[p] = pc
		}
	}
	return tics
}

// TestMultiplayerViews runs the same multiplayer game on engines looking
// through different players' eyes. The view only changes the user
// interface and sound, so the simulations must stay identical, through
// joins, leaves, deaths and respawns, and snapshots.
func TestMultiplayerViews(t *testing.T) {
	for _, mode := range []string{Coop, Deathmatch} {
		t.Run(mode, func(t *testing.T) { testViews(t, mode) })
	}
}

func testViews(t *testing.T, mode string) {
	ctx := context.Background()
	rt := testRuntime(t)
	vms := make([]*VM, MaxPlayers)
	for p := range vms {
		vm, err := rt.NewVM(ctx, Options{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		defer vm.Close()
		if err := vm.SetView(p); err != nil {
			t.Fatal(err)
		}
		vms[p] = vm
	}
	const total, step = 1500, 25
	for tic := 0; tic < total; tic += step {
		batch := netScript(tic, step)
		var want uint32
		for p, vm := range vms {
			if err := vm.NetTics(batch); err != nil {
				t.Fatal(err)
			}
			if tic%100 == 0 {
				if _, _, err := vm.Render(); err != nil {
					t.Fatal(err)
				}
			}
			sum, err := vm.Checksum()
			if err != nil {
				t.Fatal(err)
			}
			if p == 0 {
				want = sum
			} else if sum != want {
				t.Fatalf("tic %d: engine viewing player %d has checksum %08x, player 0's has %08x", tic+step, p, sum, want)
			}
		}
	}
	players, _ := vms[0].Players()
	if players != 0b1111 {
		t.Fatalf("players in game = %04b, want all four", players)
	}
	var stats []string
	for p := range MaxPlayers {
		s, _ := vms[0].PlayerStats(p)
		stats = append(stats, fmt.Sprintf("p%d health=%d kills=%d dead=%v", p, s.Health, s.Kills, s.Dead))
	}
	t.Logf("tic %d: %v", total, stats)

	// Resume from a snapshot on two engines with different views.
	vm := vms[0]
	extra := 0
	for !vm.CanSave() {
		if err := vm.NetTics(netScript(total+extra, 1)); err != nil {
			t.Fatal(err)
		}
		if extra++; extra > 350 {
			t.Fatal("never got to a point where the game can be saved")
		}
	}
	snap, err := vm.Save()
	if err != nil {
		t.Fatal(err)
	}
	var sums []uint32
	for _, view := range []int{1, 3} {
		r, err := rt.NewVM(ctx, Options{Mode: mode, Snapshot: snap})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.SetView(view)
		if p, _ := r.Players(); p != 0b1111 {
			t.Fatalf("snapshot restored players %04b", p)
		}
		start := total + extra
		for i := 0; i < 500; i += 50 {
			if err := r.NetTics(netScript(start+i, 50)); err != nil {
				t.Fatal(err)
			}
			r.Render()
		}
		sum, _ := r.Checksum()
		sums = append(sums, sum)
	}
	if sums[0] != sums[1] {
		t.Fatalf("engines resumed from the same snapshot desynced: %08x vs %08x", sums[0], sums[1])
	}
}

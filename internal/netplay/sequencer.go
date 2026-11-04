package netplay

import (
	"errors"
	"fmt"
	"time"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
	"github.com/chrsmith/temporal-doom/internal/game"
)

// Multiplayer liveness. The server has a clock, so unlike the workflow it
// can simply check these on a timer.
const (
	// AwayAfter is how long an active player can hold up everyone else
	// without sending any input before they're treated as paused.
	AwayAfter = 3 * time.Second
	// RemoveAfter is how long a player can stay away before they're
	// removed from the game (if anyone else is in it).
	RemoveAfter = 5 * time.Minute
	// StallAfter is how long someone's input has to wait before the
	// players it's waiting for are reported as holding up the game.
	StallAfter = 250 * time.Millisecond
	// MaxQueue is the most ticcmds a player can have waiting for the
	// other players (6s).
	MaxQueue = 2 * game.MaxBatch
)

var errFull = errors.New("game is full")

// seat is a player slot, plus what only the server needs to know about it.
type seat struct {
	game.Slot
	awaySince   time.Time
	lastInput   time.Time
	queuedSince time.Time       // when the queue last became non-empty
	queue       []doomvm.TicCmd // input that is waiting for the other players
}

// playing reports whether the game waits for this slot's input.
func (s *seat) playing() bool { return s.ID != "" && s.InGame && !s.Away }

// sequencer is the lockstep arbiter. Each player's input waits in a queue
// until everyone's is in, and then the merged tic runs. It's DOOM's 1993
// netcode with a star topology: clients only ever talk to the sequencer.
//
// Players who have paused (or gone quiet) are "away": the sequencer plays
// empty ticcmds for them, so their marine stands still and the game goes on
// without them. If nobody is playing, the game stops.
//
// The sequencer doesn't run DOOM. It only merges small structs, which is
// why it can live in the web server.
type sequencer struct {
	tic   int32 // the next tic to run
	slots [doomvm.MaxPlayers]seat
	// Membership changes for the engine to apply with the next tic.
	join, leave uint8
	// roster changes whenever someone joins, pauses or leaves.
	roster int
}

// newSequencer picks up a game where the workflow has it. Nobody is
// connected yet, so everyone starts out away.
func newSequencer(info game.NetInfo, now time.Time) *sequencer {
	q := &sequencer{tic: info.Tic}
	for i, s := range info.Slots {
		s.Deaths = 0
		s.Away = s.Away || s.ID != ""
		q.slots[i] = seat{Slot: s, awaySince: now}
	}
	return q
}

// slotOf returns the slot controlled by a player ID, or -1.
func (q *sequencer) slotOf(id string) int {
	for i := range q.slots {
		if id != "" && q.slots[i].ID == id {
			return i
		}
	}
	return -1
}

// playing reports whether a player ID is in the game and not away.
func (q *sequencer) playing(id string) bool {
	i := q.slotOf(id)
	return i >= 0 && q.slots[i].playing()
}

// freeSlot picks a slot for a new player: a marine nobody controls if there
// is one (slot 0's waits from the start), otherwise an empty slot.
func (q *sequencer) freeSlot() int {
	for i, s := range q.slots {
		if s.ID == "" && s.InGame {
			return i
		}
	}
	for i, s := range q.slots {
		if s.ID == "" {
			return i
		}
	}
	return -1
}

// addPlayer claims a slot (the player's own, if they had one) and makes the
// player active. Their input starts at q.tic, and the game waits for it from
// now on.
func (q *sequencer) addPlayer(id, name string, now time.Time) (int, error) {
	i := q.slotOf(id)
	if i < 0 {
		i = q.freeSlot()
	}
	if i < 0 {
		return -1, errFull
	}
	s := &q.slots[i]
	if s.ID != id {
		*s = seat{Slot: game.Slot{ID: id, InGame: s.InGame}}
	}
	if name != "" {
		s.Name = name
	}
	if s.Name == "" {
		s.Name = fmt.Sprintf("player%d", i+1)
	}
	s.Away, s.awaySince, s.lastInput = false, time.Time{}, now
	s.queue = nil // anything queued came from the client's previous session
	if !s.InGame {
		s.InGame = true
		q.join |= 1 << i
	}
	q.roster++
	return i, nil
}

// input queues a player's ticcmds.
func (q *sequencer) input(i int, cmds []doomvm.TicCmd, now time.Time) error {
	s := &q.slots[i]
	if len(s.queue)+len(cmds) > MaxQueue {
		return fmt.Errorf("too far ahead: %d tics are already waiting for the other players", len(s.queue))
	}
	if err := game.CheckCmds(q.tic+int32(len(s.queue)), cmds); err != nil {
		return err
	}
	if len(s.queue) == 0 && len(cmds) > 0 {
		s.queuedSince = now
	}
	s.queue = append(s.queue, cmds...)
	s.lastInput = now
	return nil
}

// setAway makes a player away: the game goes on, and their marine stands
// still until they join again.
func (q *sequencer) setAway(i int, now time.Time) {
	s := &q.slots[i]
	if s.Away {
		return
	}
	s.Away, s.awaySince, s.queue = true, now, nil
	q.roster++
}

// removePlayer frees a slot and takes its marine out of the game. DOOM
// can't run with nobody in the game, so the last marine stays behind,
// standing still, for the next player to claim.
func (q *sequencer) removePlayer(i int, now time.Time) {
	others := 0
	for j, s := range q.slots {
		if j != i && s.InGame {
			others++
		}
	}
	s := &q.slots[i]
	switch {
	case !s.InGame:
		*s = seat{}
	case others == 0:
		*s = seat{Slot: game.Slot{InGame: true, Name: s.Name, Away: true}, awaySince: now}
	case q.join&(1<<i) != 0:
		q.join &^= 1 << i // never made it into the engine
		*s = seat{}
	default:
		q.leave |= 1 << i
		*s = seat{}
	}
	q.roster++
}

// housekeeping treats anyone who has been holding up the game for AwayAfter
// without sending anything as paused, and removes anyone who has been away
// for RemoveAfter. It returns the slots it changed.
func (q *sequencer) housekeeping(now time.Time) (changed uint8) {
	blocking := q.waiting(now, 0)
	for i := range q.slots {
		s := &q.slots[i]
		if blocking&(1<<i) != 0 && now.Sub(s.lastInput) > AwayAfter {
			q.setAway(i, now)
			changed |= 1 << i
		}
		if s.ID != "" && s.Away && now.Sub(s.awaySince) > RemoveAfter {
			q.removePlayer(i, now)
			changed |= 1 << i
		}
	}
	return changed
}

// waiting returns a bitmask of the players holding up input that has been
// waiting for longer than after.
func (q *sequencer) waiting(now time.Time, after time.Duration) uint8 {
	var empty uint8
	stalled := false
	for j, s := range q.slots {
		if !s.playing() {
			continue
		}
		if len(s.queue) == 0 {
			empty |= 1 << j
		} else if now.Sub(s.queuedSince) >= after {
			stalled = true
		}
	}
	if !stalled {
		return 0
	}
	return empty
}

// advance runs every tic for which all active players have input, and
// returns the merged tics.
func (q *sequencer) advance(now time.Time) []doomvm.NetTic {
	var tics []doomvm.NetTic
	for {
		active, ready := 0, true
		for _, s := range q.slots {
			if s.playing() {
				active++
				ready = ready && len(s.queue) > 0
			}
		}
		if active == 0 || !ready {
			break
		}
		t := doomvm.NetTic{Join: q.join, Leave: q.leave}
		q.join, q.leave = 0, 0
		cmds := make([]doomvm.TicCmd, doomvm.MaxPlayers)
		n := 0
		for j := range q.slots {
			s := &q.slots[j]
			if s.playing() {
				cmds[j] = s.queue[0]
				s.queue = s.queue[1:]
				// Measure stalls from when the next ticcmd started waiting.
				s.queuedSince = now
			}
			if cmds[j] != (doomvm.TicCmd{}) {
				n = j + 1
			}
		}
		if n > 0 {
			t.Cmds = cmds[:n]
		}
		tics = append(tics, t)
	}
	q.tic += int32(len(tics))
	return tics
}

// rosterFor is the roster to record with a batch of tics after which the
// players in inGame are in the game. Membership changes that haven't run
// yet are left for the next batch.
func (q *sequencer) rosterFor(inGame uint8) [doomvm.MaxPlayers]game.Slot {
	var out [doomvm.MaxPlayers]game.Slot
	for i := range out {
		if inGame&(1<<i) != 0 {
			s := q.slots[i].Slot
			s.InGame, s.Deaths = true, 0
			out[i] = s
		}
	}
	return out
}

// players describes the slots for clients.
func (q *sequencer) players() []game.PlayerInfo {
	out := []game.PlayerInfo{}
	for j, s := range q.slots {
		if !s.InGame && s.ID == "" {
			continue
		}
		status := "playing"
		switch {
		case s.ID == "":
			status = "open"
		case s.Away:
			status = "away"
		}
		out = append(out, game.PlayerInfo{Slot: j, Name: s.Name, Status: status})
	}
	return out
}

// applyMembership returns who is in the game after tics, given who was in
// it before.
func applyMembership(inGame uint8, tics []doomvm.NetTic) uint8 {
	for _, t := range tics {
		inGame = (inGame &^ t.Leave) | t.Join
	}
	return inGame
}

func inGameOf(slots [doomvm.MaxPlayers]game.Slot) uint8 {
	var m uint8
	for i, s := range slots {
		if s.InGame {
			m |= 1 << i
		}
	}
	return m
}

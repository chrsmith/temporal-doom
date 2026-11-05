package game

import (
	"errors"
	"fmt"
	"strings"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/temporal-community/temporal-doom/internal/doomvm"
)

// Multiplayer works the way DOOM's own netcode did in 1993: lockstep. Every
// node runs the whole simulation, and a tic runs only once every player's
// ticcmd for it is known.
//
// The arbiter that waits for everyone's input is the web server (package
// netplay), because it's the closest point every player can reach: a round
// trip through a workflow update takes far too long to wait for on every
// tic. The server merges each tic, plays it to every client straight away,
// and records what it has played here about once a second.
//
// The workflow is the recorder and referee. It checks every recorded tic,
// runs it through its own engine, and returns the checksum so clients can
// prove they're in sync. Everything else works as in single player: the
// history is the game's demo, the leaderboard is an activity, and the game
// continues-as-new from a savegame.

// inGame returns a bitmask of the slots in the game.
func (g *game) inGame() uint8 {
	var m uint8
	for i, s := range g.net.Slots {
		if s.InGame {
			m |= 1 << i
		}
	}
	return m
}

func (g *game) registerNetHandlers(ctx workflow.Context) error {
	if err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateRecord, g.onRecord, workflow.UpdateHandlerOptions{
		Validator:   g.validateRecord,
		Description: "Record merged tics that the web server has played",
	}); err != nil {
		return err
	}
	return workflow.SetQueryHandler(ctx, QueryNet, func() (NetInfo, error) {
		return NetInfo{Over: g.over || g.quit, Segment: g.in.Segment, Tic: g.stats.Tic, Slots: g.net.Slots}, nil
	})
}

func (g *game) validateRecord(ctx workflow.Context, r NetRecord) error {
	if g.quit {
		return errors.New("game is over")
	}
	if r.StartTic != g.stats.Tic {
		return fmt.Errorf("out of sync: record starts at tic %d but the game is at tic %d", r.StartTic, g.stats.Tic)
	}
	if len(r.Tics) == 0 || len(r.Tics) > MaxRecord {
		return fmt.Errorf("record must have 1-%d tics, got %d", MaxRecord, len(r.Tics))
	}
	return checkRecord(g.inGame(), r)
}

// checkRecord is the anti-cheat part of validateRecord. The server checks
// each player's input as it arrives, but the workflow doesn't take its word
// for it: every ticcmd must be legal, and players can only join, leave and
// move in a way the engine can follow.
func checkRecord(inGame uint8, r NetRecord) error {
	for k, t := range r.Tics {
		tic := r.StartTic + int32(k)
		// The engine applies leaves, then joins, then runs the tic.
		if t.Leave&^inGame != 0 {
			return fmt.Errorf("tic %d: players %04b leave but aren't in the game", tic, t.Leave&^inGame)
		}
		inGame &^= t.Leave
		if t.Join&inGame != 0 {
			return fmt.Errorf("tic %d: players %04b join but are already in the game", tic, t.Join&inGame)
		}
		inGame |= t.Join
		if inGame == 0 {
			return fmt.Errorf("tic %d: nobody is left in the game", tic)
		}
		if len(t.Cmds) > doomvm.MaxPlayers {
			return fmt.Errorf("tic %d: %d ticcmds for %d players", tic, len(t.Cmds), doomvm.MaxPlayers)
		}
		for p, c := range t.Cmds {
			if c != (doomvm.TicCmd{}) && inGame&(1<<p) == 0 {
				return fmt.Errorf("tic %d: input for player %d, who isn't in the game", tic, p+1)
			}
			if err := checkCmd(tic, c); err != nil {
				return err
			}
		}
	}
	for p, s := range r.Slots {
		if s.InGame != (inGame&(1<<p) != 0) {
			return fmt.Errorf("roster disagrees with the tics about whether player %d is in the game", p+1)
		}
		if len(s.ID) > 64 || len(s.Name) > MaxName {
			return fmt.Errorf("player %d: ID or name too long", p+1)
		}
	}
	return nil
}

// onRecord runs a batch of merged tics. Nobody is waiting on it to play:
// the server has already sent these tics to every client. The result's
// checksum lets them check their engines against this one.
func (g *game) onRecord(ctx workflow.Context, r NetRecord) (TicResult, error) {
	// Updates can be delivered together; re-check ordering now that
	// earlier ones in the same task have been applied.
	if r.StartTic != g.stats.Tic {
		return TicResult{}, temporal.NewApplicationError(
			fmt.Sprintf("out of sync: record starts at tic %d but the game is at tic %d", r.StartTic, g.stats.Tic), "OutOfSync")
	}
	var joined uint8
	for _, t := range r.Tics {
		joined |= t.Join
	}
	g.setRoster(r.Slots, joined)
	g.updates++
	g.advance(ctx, r.Tics)
	res := TicResult{Segment: g.in.Segment, Updates: g.updates, Stats: g.stats}
	if g.w.Observer != nil {
		g.w.Observer.Batch(res)
	}
	return res, nil
}

// setRoster takes the roster from a record. Deaths are the workflow's to
// count, and start again for a marine who has just joined.
func (g *game) setRoster(slots [doomvm.MaxPlayers]Slot, joined uint8) {
	changed := false
	for i, s := range slots {
		old := g.net.Slots[i]
		s.Deaths = old.Deaths
		if joined&(1<<i) != 0 {
			s.Deaths = 0
		}
		if s != old {
			changed = true
		}
		g.net.Slots[i] = s
	}
	if changed {
		g.net.Roster++
	}
}

// advance runs merged tics through the engine.
func (g *game) advance(ctx workflow.Context, tics []doomvm.NetTic) {
	prev := g.playerStats
	if obs := g.w.Observer; obs != nil {
		// Replay tool: step one tic at a time, rendering every N tics.
		for k, t := range tics {
			g.mustNetTics([]doomvm.NetTic{t})
			tic := g.stats.Tic + int32(k) + 1
			if (g.w.RenderEvery <= 0 && k == len(tics)-1) || (g.w.RenderEvery > 0 && tic%int32(g.w.RenderEvery) == 0) {
				g.frameTic = -1
				g.render()
				s, _ := g.vm.Stats()
				obs.Frame(s, g.pixels, g.palette)
			}
		}
	} else {
		g.mustNetTics(tics)
	}
	g.netLog = append(g.netLog, tics...)
	g.collectSounds()
	g.refreshNetStats()
	g.frameTic = -1
	if !workflow.IsReplaying(ctx) {
		g.render() // keep a frame ready for QueryFrame
	}

	for j := range g.net.Slots {
		if g.net.Slots[j].InGame && !prev[j].Dead && g.playerStats[j].Dead {
			g.net.Slots[j].Deaths++
			g.deaths++
		}
	}
	if prevState := g.prevGameState; prevState == "level" && (g.stats.GameState == "intermission" || g.stats.GameState == "finale") {
		g.teamLevelCompleted()
	}
	g.upsertNetSearchAttributes(false)
}

func (g *game) mustNetTics(tics []doomvm.NetTic) {
	if err := g.vm.NetTics(tics); err != nil {
		panic(err) // see mustTics
	}
}

// refreshNetStats reads every player's stats. g.stats is the game seen by
// the lowest slot in the game, so that it doesn't depend on whose view was
// last rendered.
func (g *game) refreshNetStats() {
	g.prevGameState = g.stats.GameState
	first := -1
	for j := range g.playerStats {
		if !g.net.Slots[j].InGame {
			g.playerStats[j] = doomvm.Stats{}
			continue
		}
		s, err := g.vm.PlayerStats(j)
		if err != nil {
			panic(err)
		}
		g.playerStats[j] = s
		if first < 0 {
			first = j
		}
	}
	if first >= 0 {
		g.stats = g.playerStats[first]
	}
}

// teamLevelCompleted records a level finished in a multiplayer game. The
// whole team goes on the leaderboard together.
func (g *game) teamLevelCompleted() {
	var names []string
	var kills, items, secrets int32
	for j, s := range g.net.Slots {
		if !s.InGame {
			continue
		}
		if s.ID != "" {
			names = append(names, s.Name)
		}
		p := g.playerStats[j]
		kills, items, secrets = kills+p.Kills, items+p.Items, secrets+p.Secrets
	}
	if len(names) == 0 {
		names = []string{g.in.Config.Player}
	}
	s := g.stats
	s.Kills, s.Items, s.Secrets = kills, items, secrets
	g.recordLevel(strings.Join(names, "+"), fmt.Sprintf("E%dM%d", s.Episode, s.Map), s)
}

// players describes the slots for State.
func (g *game) players() []PlayerInfo {
	if g.net == nil {
		return nil
	}
	var out []PlayerInfo
	for j, s := range g.net.Slots {
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
		p := g.playerStats[j]
		out = append(out, PlayerInfo{Slot: j, Name: s.Name, Status: status, Health: p.Health, Kills: p.Kills, Deaths: s.Deaths})
	}
	return out
}

// upsertNetSearchAttributes is upsertSearchAttributes for a team: the
// lowest health (to find teams in trouble), and the team's kills and
// deaths.
func (g *game) upsertNetSearchAttributes(initial bool) {
	health, kills := int64(-1), int64(0)
	var names []string
	for j, s := range g.net.Slots {
		if !s.InGame {
			continue
		}
		if s.ID != "" {
			names = append(names, s.Name)
		}
		p := g.playerStats[j]
		if h := max(int64(p.Health), 0); health < 0 || h < health {
			health = h
		}
		kills += int64(p.Kills)
	}
	health = max(health, 0)
	mapName := g.stats.MapName()
	sa := &g.lastSA
	players := strings.Join(names, "\x00")
	if !initial && sa.set && sa.mapName == mapName && sa.kills == kills && sa.death == int64(g.deaths) && sa.players == players &&
		(sa.health/10 == health/10) && (health == 0) == (sa.health == 0) {
		return
	}
	updates := []temporal.SearchAttributeUpdate{
		SAMap.ValueSet(mapName),
		SAHealth.ValueSet(health),
		SAKills.ValueSet(kills),
		SADeaths.ValueSet(int64(g.deaths)),
		SAPlayers.ValueSet(names),
	}
	if initial {
		updates = append(updates, SAPlayer.ValueSet(g.in.Config.Player), SAMode.ValueSet(g.in.Config.Mode))
	}
	if err := workflow.UpsertTypedSearchAttributes(g.ctx, updates...); err != nil {
		workflow.GetLogger(g.ctx).Warn("upsert search attributes failed", "error", err)
		return
	}
	sa.set, sa.mapName, sa.health, sa.kills, sa.death, sa.players = true, mapName, health, kills, int64(g.deaths), players
}

package game

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/temporal-community/temporal-doom/internal/doomvm"
)

// Observer watches a game being simulated. The replay tool uses it to turn
// workflow histories into videos; the worker doesn't set one.
type Observer interface {
	// Frame is called with each rendered frame.
	Frame(stats doomvm.Stats, pixels, palette []byte)
	// Batch is called after each "tics" update is applied, with the same
	// result that the update returned (and that history recorded).
	Batch(res TicResult)
}

// Workflows holds the dependencies of the DOOM workflow.
type Workflows struct {
	Engine *doomvm.Runtime
	// Observer, if set, is notified of every frame (see RenderEvery).
	Observer Observer
	// RenderEvery renders a frame every N tics for the Observer. If zero,
	// frames are rendered once per update.
	RenderEvery int
}

// DoomGame runs one game of DOOM.
//
// The game state lives in a WebAssembly instance in this workflow's memory.
// It is never persisted. What Temporal persists is the input: each "tics"
// update is one entry in the log, exactly like a DOOM .lmp demo file. When a
// worker needs to rebuild the game (after a crash, a cache eviction, a
// deploy or a Reset) it replays the history, feeding the same ticcmds to a
// fresh engine, and ends up with bit-for-bit the same state.
func (w *Workflows) DoomGame(ctx workflow.Context, in GameInput) (Summary, error) {
	logger := workflow.GetLogger(ctx)
	info := workflow.GetInfo(ctx)
	cfg := in.Config

	// A game can only be replayed on the engine that recorded it, just as
	// a demo desyncs on a different version of DOOM. Record the engine
	// version in history once per run, and refuse to replay on a
	// different one. Panicking fails the workflow task, so Temporal keeps
	// retrying until a worker with the right engine picks it up. Nothing
	// is lost in the meantime.
	var recordedVersion string
	if err := workflow.SideEffect(ctx, func(workflow.Context) any {
		return w.Engine.Version
	}).Get(&recordedVersion); err != nil {
		return Summary{}, err
	}
	if recordedVersion != w.Engine.Version {
		panic(fmt.Sprintf("game was recorded with engine %s but this worker runs engine %s", recordedVersion, w.Engine.Version))
	}
	if len(in.Snapshot) > 0 && in.EngineVersion != w.Engine.Version {
		panic(fmt.Sprintf("snapshot was made by engine %s but this worker runs engine %s", in.EngineVersion, w.Engine.Version))
	}

	// Boot DOOM. This takes ~15ms, well within a workflow task.
	//
	// Engine failures panic rather than return an error. A panic fails the
	// workflow *task*, not the workflow: history stays intact and Temporal
	// retries the task, so a fixed worker can pick the game back up.
	vm, err := w.Engine.NewVM(context.Background(), cfg.options(in.Snapshot))
	if err != nil {
		panic(err)
	}
	defer vm.Close() // also runs when the workflow is evicted from the cache

	g := &game{
		w:      w,
		ctx:    ctx,
		in:     in,
		vm:     vm,
		deaths: in.Deaths,
		levels: in.Levels,
	}
	if g.stats, err = vm.Stats(); err != nil {
		panic(err)
	}
	g.startTic = g.stats.Tic
	g.frameTic = -1
	g.ticLog = []doomvm.TicCmd{}
	vm.Sounds() // discard anything from booting or loading the snapshot

	if cfg.Multiplayer() {
		g.net = in.Net
		if g.net == nil {
			// A new game: player 0's marine is already on the map,
			// waiting for the first player to join.
			g.net = &NetState{}
			g.net.Slots[0] = Slot{InGame: true, Name: cfg.Player, Away: true}
		}
		g.netLog = []doomvm.NetTic{}
		g.refreshNetStats()
		if err := g.registerNetHandlers(ctx); err != nil {
			return Summary{}, err
		}
	} else if err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateTics, g.onTics, workflow.UpdateHandlerOptions{
		Validator:   g.validateTics,
		Description: "Apply a batch of DOOM ticcmds",
	}); err != nil {
		return Summary{}, err
	}
	if err := workflow.SetUpdateHandler(ctx, UpdateQuit, func(ctx workflow.Context) (Summary, error) {
		g.quit = true
		return g.summary("quit"), nil
	}); err != nil {
		return Summary{}, err
	}
	if err := workflow.SetQueryHandler(ctx, QueryFrame, g.onFrameQuery); err != nil {
		return Summary{}, err
	}
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (State, error) {
		return State{
			Config:        cfg,
			Segment:       in.Segment,
			Updates:       g.updates,
			Stats:         g.stats,
			Deaths:        g.deaths,
			Levels:        g.levels,
			EngineVersion: w.Engine.Version,
			Players:       g.players(),
			Roster:        g.roster(),
		}, nil
	}); err != nil {
		return Summary{}, err
	}
	if err := workflow.SetQueryHandler(ctx, QuerySync, func() (Sync, error) {
		s := Sync{
			Config:        cfg,
			Segment:       in.Segment,
			Snapshot:      in.Snapshot,
			StartTic:      g.startTic,
			Tics:          g.ticLog,
			Tic:           g.stats.Tic,
			Checksum:      g.stats.Checksum,
			EngineVersion: w.Engine.Version,
		}
		if g.net != nil {
			s.Tics, s.Net = nil, g.netLog
		}
		return s, nil
	}); err != nil {
		return Summary{}, err
	}

	if in.Segment == 0 {
		if g.net != nil {
			g.upsertNetSearchAttributes(true)
		} else {
			g.upsertSearchAttributes(true)
		}
	}

	// The main loop has nothing to do with the game itself: it just waits
	// until it's time to stop or to roll over into a new run. The "clock"
	// that drives DOOM is the client, which sends ticcmds (empty ones if
	// the player is idle) at 35 tics per second.
	reason := ""
	lastSeen := -1
	for reason == "" {
		ok, err := workflow.AwaitWithTimeout(ctx, cfg.idleTimeout(), func() bool {
			return g.quit || g.shouldContinueAsNew()
		})
		if err != nil {
			return Summary{}, err
		}
		switch {
		case g.quit:
			reason = "quit"
		case !ok: // timed out; end the game if nobody has played since last time
			if g.updates == lastSeen {
				reason = "idle"
			}
			lastSeen = g.updates
		default:
			reason = "continue-as-new"
		}
	}

	// Let in-flight updates and leaderboard activities finish before
	// closing this run.
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return Summary{}, err
	}
	for _, f := range g.pending {
		if err := f.Get(ctx, nil); err != nil {
			logger.Warn("leaderboard activity failed", "error", err)
		}
	}

	if reason != "continue-as-new" {
		g.over = true
		logger.Info("game over", "reason", reason, "tic", g.stats.Tic)
		return g.summary(reason), nil
	}

	// Roll over: save the game and start a fresh run from the savegame.
	// The new run's history starts empty, so it never hits Temporal's
	// per-run limits, and replaying it only has to catch up from here.
	snapshot, err := vm.Save()
	if err != nil {
		panic(fmt.Errorf("saving game for continue-as-new: %w", err))
	}
	logger.Info("continuing as new", "segment", in.Segment+1, "tic", g.stats.Tic, "snapshotBytes", len(snapshot), "historyEvents", info.GetCurrentHistoryLength())
	return Summary{}, workflow.NewContinueAsNewError(ctx, WorkflowName, GameInput{
		Config:        cfg,
		Segment:       in.Segment + 1,
		Snapshot:      snapshot,
		EngineVersion: w.Engine.Version,
		Deaths:        g.deaths,
		Levels:        g.levels,
		Net:           g.net, // including input that's waiting for other players
	})
}

// game is the in-memory state of one workflow run.
type game struct {
	w   *Workflows
	ctx workflow.Context
	in  GameInput
	vm  *doomvm.VM

	stats    doomvm.Stats
	startTic int32
	ticLog   []doomvm.TicCmd // every tic applied in this run, for QuerySync
	updates  int
	deaths   int
	levels   []LevelResult
	quit     bool
	over     bool // the game has ended (not just this run)
	pending  []workflow.Future

	// Multiplayer only.
	net           *NetState
	netLog        []doomvm.NetTic // every tic run in this run, for QuerySync
	playerStats   [doomvm.MaxPlayers]doomvm.Stats
	prevGameState string

	frameTic  int32 // tic of the last render, -1 if stale
	frameView int   // multiplayer: whose view was rendered
	pixels    []byte
	palette   []byte
	png       []byte
	pngTic    int32

	// Recent sound events, for clients that don't run the engine
	// themselves. Like frames, sounds are derived from the inputs, so they
	// never need to be stored in history.
	sounds []doomvm.SoundEvent

	lastSA struct {
		set                  bool
		mapName              string
		health, kills, death int64
		players              string
	}
}

func (g *game) shouldContinueAsNew() bool {
	if g.updates == 0 {
		return false
	}
	if g.updates < g.in.Config.maxUpdates() && !workflow.GetInfo(g.ctx).GetContinueAsNewSuggested() {
		return false
	}
	// DOOM can only save while the player is alive in a level; otherwise
	// wait until they are.
	return g.vm.CanSave()
}

// validateTics is the update validator. Rejected updates never reach
// history, so this is where cheaters (and confused clients) get turned away.
func (g *game) validateTics(ctx workflow.Context, b TicBatch) error {
	if g.quit {
		return errors.New("game is over")
	}
	if b.StartTic != g.stats.Tic {
		return fmt.Errorf("out of sync: batch starts at tic %d but the game is at tic %d", b.StartTic, g.stats.Tic)
	}
	if len(b.Cmds) == 0 || len(b.Cmds) > MaxBatch {
		return fmt.Errorf("batch must have 1-%d tics, got %d", MaxBatch, len(b.Cmds))
	}
	return CheckCmds(b.StartTic, b.Cmds)
}

// CheckCmds is the anti-cheat part of the validators: it rejects ticcmds
// that DOOM's own input code could never produce. The multiplayer server
// uses it too, on each player's input as it arrives.
func CheckCmds(startTic int32, cmds []doomvm.TicCmd) error {
	for i, c := range cmds {
		if err := checkCmd(startTic+int32(i), c); err != nil {
			return err
		}
	}
	return nil
}

func checkCmd(tic int32, c doomvm.TicCmd) error {
	if c.Forward > MaxMove || c.Forward < -MaxMove || c.Side > MaxMove || c.Side < -MaxMove {
		return fmt.Errorf("tic %d: impossible movement (%d, %d)", tic, c.Forward, c.Side)
	}
	if c.Buttons&doomvm.ButtonSpecial != 0 {
		return fmt.Errorf("tic %d: special buttons (pause/save) are not allowed", tic)
	}
	return nil
}

// onTics is the update handler: run the batch through the engine.
func (g *game) onTics(ctx workflow.Context, b TicBatch) (TicResult, error) {
	// Updates can be delivered together; re-check ordering now that
	// earlier ones in the same task have been applied.
	if b.StartTic != g.stats.Tic {
		return TicResult{}, temporal.NewApplicationError(
			fmt.Sprintf("out of sync: batch starts at tic %d but the game is at tic %d", b.StartTic, g.stats.Tic), "OutOfSync")
	}
	prev := g.stats

	if obs := g.w.Observer; obs != nil {
		// Replay tool: step one tic at a time, rendering every N tics.
		for i, cmd := range b.Cmds {
			g.mustTics([]doomvm.TicCmd{cmd})
			tic := b.StartTic + int32(i) + 1
			if (g.w.RenderEvery <= 0 && i == len(b.Cmds)-1) || (g.w.RenderEvery > 0 && tic%int32(g.w.RenderEvery) == 0) {
				g.stats.Tic = tic
				g.render()
				s, _ := g.vm.Stats()
				obs.Frame(s, g.pixels, g.palette)
			}
		}
	} else {
		g.mustTics(b.Cmds)
	}
	g.ticLog = append(g.ticLog, b.Cmds...)
	g.collectSounds()
	g.updates++
	stats, err := g.vm.Stats()
	if err != nil {
		panic(err)
	}
	g.stats = stats
	g.frameTic = -1
	if !workflow.IsReplaying(ctx) {
		// Keep a frame ready for QueryFrame. When replaying there's no
		// one to look at it, so skip the work; QueryFrame renders lazily.
		g.render()
	}

	if !prev.Dead && stats.Dead {
		g.deaths++
	}
	// Finishing a level shows the intermission screen, except E1M8, which
	// goes straight to the end-of-episode finale.
	if prev.GameState == "level" && (stats.GameState == "intermission" || stats.GameState == "finale") {
		g.levelCompleted(prev.MapName(), stats)
	}
	g.upsertSearchAttributes(false)

	res := TicResult{Segment: g.in.Segment, Updates: g.updates, Stats: stats}
	if g.w.Observer != nil {
		g.w.Observer.Batch(res)
	}
	return res, nil
}

// soundHistoryTics is how far back QueryFrame can return sounds (5s).
const soundHistoryTics = 5 * 35

func (g *game) collectSounds() {
	ev, err := g.vm.Sounds()
	if err != nil {
		panic(err)
	}
	g.sounds = append(g.sounds, ev...)
	if n := len(g.sounds); n > 0 {
		oldest := g.sounds[n-1].Tic - soundHistoryTics
		i := 0
		for i < n && g.sounds[i].Tic < oldest {
			i++
		}
		g.sounds = g.sounds[i:]
	}
}

func (g *game) mustTics(cmds []doomvm.TicCmd) {
	if err := g.vm.Tics(cmds); err != nil {
		// The engine trapped. Its state is unknowable, so fail the
		// workflow task rather than the update; the history is intact
		// and the game can be recovered with a newer engine or a Reset.
		panic(err)
	}
}

func (g *game) render() {
	if g.frameTic == g.stats.Tic && g.pixels != nil {
		return
	}
	pixels, palette, err := g.vm.Render()
	if err != nil {
		panic(err)
	}
	g.pixels, g.palette, g.frameTic = pixels, palette, g.stats.Tic
}

// onFrameQuery serves the current frame. Queries aren't recorded in history,
// which is why frames are served here rather than in update results: a
// 64KB frame per update would blow through the 50MB history limit in
// minutes.
//
// In a multiplayer game, view chooses whose eyes to look through.
func (g *game) onFrameQuery(sinceTic int32, view int) (Frame, error) {
	if g.net != nil && view != g.frameView && view >= 0 && view < doomvm.MaxPlayers {
		// The view only affects what's drawn (and heard), never the
		// simulation, so it's safe to change from a query.
		if err := g.vm.SetView(view); err != nil {
			return Frame{}, err
		}
		g.frameView, g.frameTic = view, -1
	}
	if g.frameTic != g.stats.Tic || g.pixels == nil {
		// Rendering only touches renderer state, not the simulation, so
		// it's safe to do lazily from a query.
		g.render()
	}
	if g.png == nil || g.pngTic != g.frameTic {
		b, err := EncodePNG(g.pixels, g.palette)
		if err != nil {
			return Frame{}, err
		}
		g.png, g.pngTic = b, g.frameTic
	}
	fr := Frame{Tic: g.stats.Tic, Stats: g.stats, PNG: g.png}
	for _, ev := range g.sounds {
		if ev.Tic > sinceTic {
			fr.Sounds = append(fr.Sounds, ev)
		}
	}
	return fr, nil
}

func (g *game) levelCompleted(mapName string, s doomvm.Stats) {
	g.recordLevel(g.in.Config.Player, mapName, s)
}

func (g *game) recordLevel(player, mapName string, s doomvm.Stats) {
	r := LevelResult{
		WorkflowID:   workflow.GetInfo(g.ctx).WorkflowExecution.ID,
		Player:       player,
		Map:          mapName,
		Skill:        int(s.Skill) + 1,
		Kills:        s.Kills,
		TotalKills:   s.TotalKills,
		Items:        s.Items,
		TotalItems:   s.TotalItems,
		Secrets:      s.Secrets,
		TotalSecrets: s.TotalSecrets,
		TimeTics:     s.LevelTime,
		Completed:    workflow.Now(g.ctx),
	}
	g.levels = append(g.levels, r)
	// Posting to the leaderboard is a side effect, so it's an activity.
	// Don't block the update on it; the main loop waits for it before the
	// run ends.
	actx := workflow.WithActivityOptions(g.ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Second})
	var a *Activities
	g.pending = append(g.pending, workflow.ExecuteActivity(actx, a.RecordLevel, r))
}

// upsertSearchAttributes publishes game state to Temporal visibility. Each
// upsert is a history event, so only publish meaningful changes.
func (g *game) upsertSearchAttributes(initial bool) {
	s := g.stats
	mapName := s.MapName()
	health := int64(s.Health)
	if health < 0 {
		health = 0
	}
	sa := &g.lastSA
	if sa.set && sa.mapName == mapName && sa.kills == int64(s.Kills) && sa.death == int64(g.deaths) &&
		(sa.health/10 == health/10) && (health == 0) == (sa.health == 0) {
		return
	}
	updates := []temporal.SearchAttributeUpdate{
		SAMap.ValueSet(mapName),
		SAHealth.ValueSet(health),
		SAKills.ValueSet(int64(s.Kills)),
		SADeaths.ValueSet(int64(g.deaths)),
	}
	if initial {
		updates = append(updates, SAPlayer.ValueSet(g.in.Config.Player))
	}
	if err := workflow.UpsertTypedSearchAttributes(g.ctx, updates...); err != nil {
		workflow.GetLogger(g.ctx).Warn("upsert search attributes failed", "error", err)
		return
	}
	sa.set, sa.mapName, sa.health, sa.kills, sa.death = true, mapName, health, int64(s.Kills), int64(g.deaths)
}

// roster changes whenever the player list does.
func (g *game) roster() int {
	if g.net == nil {
		return 0
	}
	return g.net.Roster
}

func (g *game) summary(reason string) Summary {
	return Summary{Reason: reason, Stats: g.stats, Deaths: g.deaths, Levels: g.levels}
}

// EncodePNG encodes a palette-indexed frame.
func EncodePNG(pixels, palette []byte) ([]byte, error) {
	img := FrameImage(pixels, palette)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FrameImage wraps a palette-indexed frame as an image.
func FrameImage(pixels, palette []byte) *image.Paletted {
	pal := make(color.Palette, 256)
	for i := range pal {
		pal[i] = color.RGBA{palette[i*3], palette[i*3+1], palette[i*3+2], 0xff}
	}
	return &image.Paletted{
		Pix:     pixels,
		Stride:  doomvm.Width,
		Rect:    image.Rect(0, 0, doomvm.Width, doomvm.Height),
		Palette: pal,
	}
}

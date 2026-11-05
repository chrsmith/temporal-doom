// Package game implements DOOM as a Temporal workflow.
package game

import (
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/temporal-community/temporal-doom/internal/doomvm"
)

const (
	TaskQueue    = "doom"
	WorkflowName = "DoomGame"

	// UpdateTics carries a batch of ticcmds. It is the only way to change
	// the game.
	UpdateTics = "tics"
	// UpdateQuit ends the game.
	UpdateQuit = "quit"

	// UpdateRecord carries a batch of merged tics in a multiplayer game.
	// The web server sequences multiplayer games in real time (see package
	// netplay) and records what it has played about once a second.
	UpdateRecord = "record"

	// QueryFrame returns the latest rendered frame as a PNG, plus the sound
	// events after the tic given as its argument.
	QueryFrame = "frame"
	// QueryState returns the current Stats and bookkeeping.
	QueryState = "state"
	// QuerySync returns everything a client needs to reproduce the current
	// game state bit-for-bit: this run's starting snapshot and every
	// ticcmd since.
	QuerySync = "sync"
	// QueryNet returns a multiplayer game's roster, including the player
	// IDs, for the web server. It isn't exposed to browsers.
	QueryNet = "net"

	// MaxBatch is the largest number of tics accepted in one update (3s).
	MaxBatch = 105
	// MaxMove is the largest legal forward/side movement (DOOM's
	// MAXPLMOVE / TURBOTHRESHOLD).
	MaxMove = 0x32
	// MaxRecord is the most merged tics accepted in one "record" update
	// (10s).
	MaxRecord = 350
	// MaxName is the longest player name.
	MaxName = 24
)

// Defaults for GameConfig.
const (
	DefaultMaxUpdatesPerRun = 1000
	// A multiplayer game records about once a second, so this rolls over
	// every 5 minutes or so. Any longer and the sync query, which returns
	// every tic in the run, gets close to gRPC's 4MB limit with 4 players.
	DefaultMaxRecordsPerRun = 300
	DefaultIdleTimeout      = 30 * time.Minute
)

// Search attributes, so the Temporal UI can find e.g. every game currently
// on E1M8 with less than 20 health. Registered by the worker on startup.
var (
	SAPlayer = temporal.NewSearchAttributeKeyKeyword("DoomPlayer")
	SAMap    = temporal.NewSearchAttributeKeyKeyword("DoomMap")
	SAHealth = temporal.NewSearchAttributeKeyInt64("DoomHealth")
	SAKills  = temporal.NewSearchAttributeKeyInt64("DoomKills")
	SADeaths = temporal.NewSearchAttributeKeyInt64("DoomDeaths")
	// Multiplayer: the game mode, and the names of the players in it
	// (e.g. DoomPlayers = 'alice').
	SAMode    = temporal.NewSearchAttributeKeyKeyword("DoomMode")
	SAPlayers = temporal.NewSearchAttributeKeyKeywordList("DoomPlayers")
)

// GameConfig is fixed for the lifetime of a game.
type GameConfig struct {
	Player  string `json:"player"`
	Skill   int    `json:"skill"` // 1 (I'm too young to die) - 5 (Nightmare!)
	Episode int    `json:"episode"`
	Map     int    `json:"map"`
	// Mode is "" for a single-player game, or "coop" or "deathmatch" for
	// up to 4 players who can come and go.
	Mode string `json:"mode,omitempty"`
	// MaxUpdatesPerRun bounds the history of each run; after this many
	// updates the workflow continues-as-new from a savegame.
	MaxUpdatesPerRun int `json:"maxUpdatesPerRun,omitempty"`
	// IdleTimeout ends the game if no input arrives for this long.
	IdleTimeout time.Duration `json:"idleTimeout,omitempty"`
}

// Multiplayer reports whether players can join the game.
func (c GameConfig) Multiplayer() bool { return c.Mode != doomvm.Single }

func (c GameConfig) maxUpdates() int {
	switch {
	case c.MaxUpdatesPerRun > 0:
		return c.MaxUpdatesPerRun
	case c.Multiplayer():
		return DefaultMaxRecordsPerRun
	}
	return DefaultMaxUpdatesPerRun
}

func (c GameConfig) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return DefaultIdleTimeout
}

func (c GameConfig) options(snapshot []byte) doomvm.Options {
	return doomvm.Options{Skill: c.Skill, Episode: c.Episode, Map: c.Map, Mode: c.Mode, Snapshot: snapshot}
}

// GameInput is the workflow input. Everything except Config is carried over
// from the previous run when the workflow continues-as-new.
type GameInput struct {
	Config GameConfig `json:"config"`
	// Segment counts continue-as-new generations, starting at 0.
	Segment int `json:"segment"`
	// Snapshot is a DOOM savegame to start this run from. Empty for a new game.
	Snapshot []byte `json:"snapshot,omitempty"`
	// EngineVersion is the engine+WAD version that produced Snapshot.
	EngineVersion string `json:"engineVersion,omitempty"`
	// Totals carried across runs.
	Deaths int           `json:"deaths"`
	Levels []LevelResult `json:"levels,omitempty"`
	// Net is the multiplayer state, including input that has been
	// acknowledged but not run yet.
	Net *NetState `json:"net,omitempty"`
}

// NetState is the workflow's view of a multiplayer game: who is in it, as
// of the last recorded tic.
type NetState struct {
	Slots [doomvm.MaxPlayers]Slot `json:"slots"`
	// Roster changes whenever someone joins, pauses or leaves, so clients
	// know when to refresh their player list.
	Roster int `json:"roster"`
}

// Slot is one of DOOM's 4 player slots.
type Slot struct {
	// ID identifies the client that controls the slot; empty if nobody
	// does. A slot can be in the game without an ID: slot 0's marine is
	// there from the start, waiting for the first player to join.
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	InGame bool   `json:"inGame,omitempty"`
	// Away players (paused, or gone quiet) don't hold up the game; the
	// server plays empty ticcmds for them.
	Away bool `json:"away,omitempty"`
	// Deaths is counted by the workflow; it's ignored in a NetRecord.
	Deaths int `json:"deaths,omitempty"`
}

// NetRecord is the payload of the "record" update: tics the web server has
// already played, starting at the game's current tic.
type NetRecord struct {
	// StartTic must equal the game's current tic, which makes records
	// idempotent, as in TicBatch.
	StartTic int32           `json:"startTic"`
	Tics     []doomvm.NetTic `json:"tics"`
	// Slots is the roster after the last tic. Which slots are in the game
	// must agree with the joins and leaves in Tics.
	Slots [doomvm.MaxPlayers]Slot `json:"slots"`
}

// NetInfo is returned by QueryNet.
type NetInfo struct {
	// Over is set once the game has ended. (Queries still work on a
	// finished workflow, by replaying it.)
	Over    bool                    `json:"over,omitempty"`
	Segment int                     `json:"segment"`
	Tic     int32                   `json:"tic"`
	Slots   [doomvm.MaxPlayers]Slot `json:"slots"`
}

// PlayerInfo describes a slot in State. It deliberately leaves out the ID,
// which is what lets a client control the slot.
type PlayerInfo struct {
	Slot   int    `json:"slot"`
	Name   string `json:"name"`
	Status string `json:"status"` // "playing", "away", or "open" (in the game, but nobody controls it)
	Health int32  `json:"health"`
	Kills  int32  `json:"kills"`
	Deaths int    `json:"deaths"`
}

// TicBatch is the payload of the "tics" update.
type TicBatch struct {
	// StartTic must equal the game's current tic. This makes updates
	// idempotent (a retried batch is rejected rather than applied twice)
	// and lets clients detect that they've fallen out of sync.
	StartTic int32           `json:"startTic"`
	Cmds     []doomvm.TicCmd `json:"cmds"`
}

// TicResult is returned by the "tics" and "record" updates. It is
// deliberately tiny: update results are stored in workflow history, so
// frames are served by QueryFrame instead.
type TicResult struct {
	Segment int          `json:"segment"`
	Updates int          `json:"updates"` // updates applied in this run
	Stats   doomvm.Stats `json:"stats"`   // multiplayer: from the lowest slot in the game's point of view
}

// Frame is returned by QueryFrame.
type Frame struct {
	Tic   int32        `json:"tic"`
	Stats doomvm.Stats `json:"stats"`
	PNG   []byte       `json:"png"`
	// Sounds produced after the tic the client asked about (at most the
	// last few seconds' worth).
	Sounds []doomvm.SoundEvent `json:"sounds,omitempty"`
}

// State is returned by QueryState.
type State struct {
	Config        GameConfig    `json:"config"`
	Segment       int           `json:"segment"`
	Updates       int           `json:"updates"`
	Stats         doomvm.Stats  `json:"stats"`
	Deaths        int           `json:"deaths"`
	Levels        []LevelResult `json:"levels,omitempty"`
	EngineVersion string        `json:"engineVersion"`
	Players       []PlayerInfo  `json:"players,omitempty"` // multiplayer only
	Roster        int           `json:"roster,omitempty"`
}

// Sync is returned by QuerySync. Starting a fresh engine with Options,
// loading Snapshot and running Tics reproduces the game's current state
// exactly. That's precisely what the worker does when it replays this run's
// history.
type Sync struct {
	Config        GameConfig      `json:"config"`
	Segment       int             `json:"segment"`
	Snapshot      []byte          `json:"snapshot,omitempty"`
	StartTic      int32           `json:"startTic"`
	Tics          []doomvm.TicCmd `json:"tics"`
	Net           []doomvm.NetTic `json:"net,omitempty"` // instead of Tics, in a multiplayer game
	Tic           int32           `json:"tic"`
	Checksum      uint32          `json:"checksum"`
	EngineVersion string          `json:"engineVersion"`
}

// LevelResult is recorded when the player finishes a level.
type LevelResult struct {
	WorkflowID   string    `json:"workflowId"`
	Player       string    `json:"player"`
	Map          string    `json:"map"`
	Skill        int       `json:"skill"`
	Kills        int32     `json:"kills"`
	TotalKills   int32     `json:"totalKills"`
	Items        int32     `json:"items"`
	TotalItems   int32     `json:"totalItems"`
	Secrets      int32     `json:"secrets"`
	TotalSecrets int32     `json:"totalSecrets"`
	TimeTics     int32     `json:"timeTics"`
	Completed    time.Time `json:"completed"`
}

// Summary is the workflow result.
type Summary struct {
	Reason string        `json:"reason"`
	Stats  doomvm.Stats  `json:"stats"`
	Deaths int           `json:"deaths"`
	Levels []LevelResult `json:"levels,omitempty"`
}

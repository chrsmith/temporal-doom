// Package doomvm runs the DOOM engine (build/doom.wasm) in a wazero sandbox.
//
// Each VM is an isolated WebAssembly instance with its own memory, so one
// worker process can host many games at once even though the engine itself
// is full of global variables. More importantly for Temporal, the sandbox
// has no real clock, no filesystem, no network and a fixed "random" source:
// the only way to influence the game is through the ticcmds the host feeds
// it. That makes the engine safe to run inside workflow code.
package doomvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	Width      = 320
	Height     = 200
	FrameSize  = Width * Height
	ticCmdSize = 8                         // must match engine.TicCmdSize
	netTicSize = 4 + MaxPlayers*ticCmdSize // must match engine.NetTicSize

	// MaxPlayers is DOOM's player limit.
	MaxPlayers = 4
)

// DOOM's release date. It is what the engine sees if it ever asks the time.
const doomEpoch = 755481600 // 1993-12-10T00:00:00Z

// Button bits for TicCmd.Buttons.
const (
	ButtonAttack      = 1
	ButtonUse         = 2
	ButtonChange      = 4
	ButtonWeaponMask  = 0x38
	ButtonWeaponShift = 3
	ButtonSpecial     = 0x80
)

// TicCmd is one tic (1/35s) of player input, mirroring DOOM's ticcmd_t.
type TicCmd struct {
	Forward   int8  `json:"f,omitempty"`
	Side      int8  `json:"s,omitempty"`
	AngleTurn int16 `json:"t,omitempty"`
	Buttons   uint8 `json:"b,omitempty"`
}

// NetTic is one tic of a multiplayer game, mirroring gore.NetTic: every
// player's input, plus the players leaving and joining before it runs.
type NetTic struct {
	// Cmds is indexed by player. It may be shorter than MaxPlayers; the
	// missing players' commands are empty.
	Cmds  []TicCmd `json:"c,omitempty"`
	Leave uint8    `json:"l,omitempty"`
	Join  uint8    `json:"j,omitempty"`
}

// SoundEvent mirrors gore.SoundEvent: something the player should hear.
type SoundEvent struct {
	Tic     int32  `json:"tic"`            // the tic whose simulation produced it
	Kind    string `json:"kind"`           // "start", "update" or "stop"
	Channel int32  `json:"ch"`             // DOOM mixer channel
	Name    string `json:"name,omitempty"` // "start" only; samples are in WAD lump "DS"+upper(Name)
	Volume  int32  `json:"vol,omitempty"`  // 0-127
	Sep     int32  `json:"sep,omitempty"`  // stereo separation: 0 left, 128 centre, 254 right
}

// Stats mirrors gore.Stats.
type Stats struct {
	Tic          int32  `json:"tic"`
	LevelTime    int32  `json:"levelTime"`
	GameState    string `json:"gameState"`
	Skill        int32  `json:"skill"`
	Episode      int32  `json:"episode"`
	Map          int32  `json:"map"`
	Health       int32  `json:"health"`
	Armor        int32  `json:"armor"`
	Weapon       int32  `json:"weapon"`
	Ammo         int32  `json:"ammo"`
	Dead         bool   `json:"dead"`
	Kills        int32  `json:"kills"`
	TotalKills   int32  `json:"totalKills"`
	Items        int32  `json:"items"`
	TotalItems   int32  `json:"totalItems"`
	Secrets      int32  `json:"secrets"`
	TotalSecrets int32  `json:"totalSecrets"`
	Checksum     uint32 `json:"checksum"`
}

// MapName returns e.g. "E1M3".
func (s Stats) MapName() string { return fmt.Sprintf("E%dM%d", s.Episode, s.Map) }

// Runtime holds the compiled engine and the WAD. It is safe for concurrent
// use and should be shared by all VMs in a process.
type Runtime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	wad      []byte
	// Version identifies the engine+WAD combination. Games are only
	// reproducible on the exact same engine and WAD, just like demos.
	Version string

	mu sync.Mutex // serialises instantiation
}

// NewRuntime compiles the engine. cacheDir, if non-empty, is used to cache
// the compiled machine code between process restarts.
func NewRuntime(ctx context.Context, wasm, wad []byte, cacheDir string) (*Runtime, error) {
	cfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(8192) // 512 MiB
	if cacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
		if err != nil {
			return nil, err
		}
		cfg = cfg.WithCompilationCache(cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return nil, err
	}
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compiling engine: %w", err)
	}
	return &Runtime{
		rt:       rt,
		compiled: compiled,
		wad:      wad,
		Version:  VersionOf(wasm, wad),
	}, nil
}

func (r *Runtime) Close(ctx context.Context) error { return r.rt.Close(ctx) }

// Game modes.
const (
	Single     = ""
	Coop       = "coop"
	Deathmatch = "deathmatch"
)

// Options configure a new game.
type Options struct {
	Skill   int // 1-5
	Episode int
	Map     int
	// Mode is Single, Coop or Deathmatch. Multiplayer games are driven
	// with NetTics instead of Tics.
	Mode string
	// Snapshot, if set, is a savegame (from VM.Save) to resume from.
	Snapshot []byte
	// Stdout receives the engine's console output. Discarded if nil.
	Stdout io.Writer
}

// Args returns the DOOM command line for these options.
func (o Options) Args() []string {
	skill, ep, mp := o.Skill, o.Episode, o.Map
	if skill == 0 {
		skill = 3
	}
	if ep == 0 {
		ep = 1
	}
	if mp == 0 {
		mp = 1
	}
	args := []string{"-skill", strconv.Itoa(skill), "-warp", strconv.Itoa(ep), strconv.Itoa(mp)}
	switch o.Mode {
	case Coop:
		args = append(args, "-solo-net") // netgame rules, no network
	case Deathmatch:
		args = append(args, "-solo-net", "-deathmatch")
	}
	return args
}

// MapName returns the starting map, e.g. "E1M1".
func (o Options) MapName() string {
	args := o.Args()
	return fmt.Sprintf("E%sM%s", args[3], args[4])
}

// VM is one running game. It is not safe for concurrent use.
type VM struct {
	mod api.Module
	mem api.Memory
	fn  map[string]api.Function
	ctx context.Context
}

// deterministicClock is a monotonic clock that advances by 1ms per reading,
// starting at doomEpoch. The count of readings is itself deterministic, so
// the clock is too.
type deterministicClock struct{ ns int64 }

func (c *deterministicClock) walltime() (int64, int32) {
	c.ns += 1e6
	t := doomEpoch*1e9 + c.ns
	return t / 1e9, int32(t % 1e9)
}

func (c *deterministicClock) nanotime() int64 {
	c.ns += 1e6
	return c.ns
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// NewVM instantiates and initialises a new engine.
func (r *Runtime) NewVM(ctx context.Context, opts Options) (*VM, error) {
	clock := &deterministicClock{}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous, so many instances can coexist
		WithStartFunctions("_initialize").
		WithStdout(stdout).
		WithStderr(stdout).
		WithWalltime(clock.walltime, 1).
		WithNanotime(clock.nanotime, 1).
		WithNanosleep(func(int64) {}).
		WithRandSource(zeroReader{})
	// No WithFS*: the engine has no filesystem at all.

	r.mu.Lock()
	mod, err := r.rt.InstantiateModule(ctx, r.compiled, cfg)
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("instantiating engine: %w", err)
	}
	vm := &VM{mod: mod, mem: mod.Memory(), fn: map[string]api.Function{}, ctx: ctx}
	for _, name := range []string{"doom_alloc", "doom_out", "doom_wad", "doom_init", "doom_load", "doom_tics", "doom_net_tics", "doom_view", "doom_players", "doom_render", "doom_stats", "doom_player_stats", "doom_checksum", "doom_can_save", "doom_save", "doom_sounds"} {
		f := mod.ExportedFunction(name)
		if f == nil {
			mod.Close(ctx)
			return nil, fmt.Errorf("engine is missing export %q", name)
		}
		vm.fn[name] = f
	}
	if err := vm.input(r.wad); err != nil {
		vm.Close()
		return nil, err
	}
	if _, err := vm.call("doom_wad", uint64(len(r.wad))); err != nil {
		vm.Close()
		return nil, err
	}
	args := []byte(joinNUL(opts.Args()))
	if err := vm.input(args); err != nil {
		vm.Close()
		return nil, err
	}
	if _, err := vm.call("doom_init", uint64(len(args))); err != nil {
		vm.Close()
		return nil, fmt.Errorf("engine init: %w", err)
	}
	if len(opts.Snapshot) > 0 {
		if err := vm.input(opts.Snapshot); err != nil {
			vm.Close()
			return nil, err
		}
		n, err := vm.call("doom_load", uint64(len(opts.Snapshot)))
		if err == nil && int32(n) < 0 {
			err = errors.New(string(vm.output(-int32(n))))
		}
		if err != nil {
			vm.Close()
			return nil, fmt.Errorf("loading snapshot: %w", err)
		}
	}
	return vm, nil
}

func joinNUL(args []string) string {
	var b bytes.Buffer
	for i, a := range args {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(a)
	}
	return b.String()
}

func (vm *VM) call(name string, params ...uint64) (uint64, error) {
	res, err := vm.fn[name].Call(vm.ctx, params...)
	if err != nil {
		return 0, fmt.Errorf("engine %s: %w", name, err)
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

// input copies data into a freshly allocated engine buffer.
func (vm *VM) input(data []byte) error {
	p, err := vm.call("doom_alloc", uint64(len(data)))
	if err != nil {
		return err
	}
	if len(data) > 0 && !vm.mem.Write(uint32(p), data) {
		return errors.New("engine: input buffer out of range")
	}
	return nil
}

// output returns a copy of the engine's output buffer.
func (vm *VM) output(n int32) []byte {
	p, err := vm.call("doom_out")
	if err != nil || n <= 0 {
		return nil
	}
	b, ok := vm.mem.Read(uint32(p), uint32(n))
	if !ok {
		return nil
	}
	return bytes.Clone(b)
}

// Tics runs one tic per command.
func (vm *VM) Tics(cmds []TicCmd) error {
	if len(cmds) == 0 {
		return nil
	}
	buf := make([]byte, len(cmds)*ticCmdSize)
	for i, c := range cmds {
		putTicCmd(buf[i*ticCmdSize:], c)
	}
	if err := vm.input(buf); err != nil {
		return err
	}
	_, err := vm.call("doom_tics", uint64(len(cmds)))
	return err
}

// NetTics runs one multiplayer tic per entry.
func (vm *VM) NetTics(tics []NetTic) error {
	if len(tics) == 0 {
		return nil
	}
	buf := make([]byte, len(tics)*netTicSize)
	for i, t := range tics {
		b := buf[i*netTicSize:]
		b[0], b[1] = t.Leave, t.Join
		for p, c := range t.Cmds[:min(len(t.Cmds), MaxPlayers)] {
			putTicCmd(b[4+p*ticCmdSize:], c)
		}
	}
	if err := vm.input(buf); err != nil {
		return err
	}
	_, err := vm.call("doom_net_tics", uint64(len(tics)))
	return err
}

// SetView chooses whose view Render draws. Until that player has a body
// (they join on the next tic), Render shows someone who does. The view
// never affects the simulation.
func (vm *VM) SetView(player int) error {
	_, err := vm.call("doom_view", uint64(player))
	return err
}

// Players returns a bitmask of the players in the game.
func (vm *VM) Players() (uint8, error) {
	n, err := vm.call("doom_players")
	return uint8(n), err
}

// PlayerStats returns a snapshot of the game state from a player's point of
// view.
func (vm *VM) PlayerStats(player int) (Stats, error) {
	var s Stats
	n, err := vm.call("doom_player_stats", uint64(player))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(vm.output(int32(n)), &s)
	return s, err
}

func putTicCmd(b []byte, c TicCmd) {
	b[0] = byte(c.Forward)
	b[1] = byte(c.Side)
	binary.LittleEndian.PutUint16(b[2:], uint16(c.AngleTurn))
	b[4] = c.Buttons
}

// Render draws a frame and returns the palette-indexed pixels (320x200) and
// the 768 byte RGB palette.
func (vm *VM) Render() (pixels, palette []byte, err error) {
	n, err := vm.call("doom_render")
	if err != nil {
		return nil, nil, err
	}
	out := vm.output(int32(n))
	if len(out) != FrameSize+768 {
		return nil, nil, fmt.Errorf("engine: bad frame size %d", len(out))
	}
	return out[:FrameSize], out[FrameSize:], nil
}

// Stats returns a snapshot of the game state.
func (vm *VM) Stats() (Stats, error) {
	var s Stats
	n, err := vm.call("doom_stats")
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(vm.output(int32(n)), &s)
	return s, err
}

// Checksum returns the engine's simulation checksum.
func (vm *VM) Checksum() (uint32, error) {
	n, err := vm.call("doom_checksum")
	return uint32(n), err
}

// Sounds returns the sound events produced since the last call.
func (vm *VM) Sounds() ([]SoundEvent, error) {
	n, err := vm.call("doom_sounds")
	if err != nil || n == 0 {
		return nil, err
	}
	var ev []SoundEvent
	err = json.Unmarshal(vm.output(int32(n)), &ev)
	return ev, err
}

// CanSave reports whether Save will succeed right now.
func (vm *VM) CanSave() bool {
	n, err := vm.call("doom_can_save")
	return err == nil && n == 1
}

// Save returns a savegame that NewVM can resume from.
func (vm *VM) Save() ([]byte, error) {
	n, err := vm.call("doom_save")
	if err != nil {
		return nil, err
	}
	if int32(n) < 0 {
		return nil, errors.New(string(vm.output(-int32(n))))
	}
	return vm.output(int32(n)), nil
}

// Close releases the instance and its memory.
func (vm *VM) Close() {
	if vm.mod != nil {
		vm.mod.Close(context.Background())
		vm.mod = nil
	}
}

// VersionOf returns the Version a Runtime would report for an engine and WAD.
func VersionOf(wasm, wad []byte) string {
	h := sha256.New()
	h.Write(wasm)
	h.Write(wad)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

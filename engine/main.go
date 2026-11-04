//go:build wasip1

// Command engine is DOOM (via gore) compiled to a WASI reactor module.
//
// Build with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o build/doom.wasm ./engine
//
// The same module runs in two places: inside Temporal workflow code (hosted
// by wazero, see internal/doomvm) and in the browser (hosted by web/wasi.js).
// Because it is the same binary, both sides simulate bit-for-bit identical
// games from the same inputs.
//
// ABI: the host copies inputs into a buffer obtained from doom_alloc, then
// calls a function passing the input length. Functions that produce output
// write it to a buffer whose address is returned by doom_out, and return its
// length.
package main

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing/fstest"
	"unsafe"

	"github.com/AndreRenaud/gore"
)

// TicCmdSize is the size of one packed ticcmd in doom_tics' input:
// forward int8, side int8, angleturn int16 (little endian), buttons uint8,
// then 3 bytes of padding.
const TicCmdSize = 8

// NetTicSize is the size of one packed tic in doom_net_tics' input: a leave
// mask, a join mask and 2 bytes of padding, then one packed ticcmd per player.
const NetTicSize = 4 + 4*TicCmdSize

var (
	in  []byte // host -> engine
	out []byte // engine -> host
	wad []byte
)

func main() {}

func ptr(b []byte) int32 {
	if len(b) == 0 {
		return 0
	}
	return int32(uintptr(unsafe.Pointer(&b[0])))
}

//go:wasmexport doom_alloc
func doomAlloc(n int32) int32 {
	in = make([]byte, n)
	return ptr(in)
}

//go:wasmexport doom_out
func doomOut() int32 {
	return ptr(out)
}

// doom_wad takes ownership of the input buffer as the IWAD contents.
//
//go:wasmexport doom_wad
func doomWAD(n int32) {
	wad = in[:n]
	in = nil
	gore.SetVirtualFileSystem(fstest.MapFS{"doom.wad": {Data: wad}})
}

// doom_init starts the engine; the input is NUL separated command line args.
// "-iwad doom.wad" is implied.
//
//go:wasmexport doom_init
func doomInit(n int32) {
	args := []string{"-iwad", "doom.wad", "-nogui"}
	if n > 0 {
		args = append(args, strings.Split(string(in[:n]), "\x00")...)
	}
	gore.StepInit(nil, args)
}

//go:wasmexport doom_load
func doomLoad(n int32) int32 {
	if err := gore.LoadGameBytes(in[:n]); err != nil {
		out = []byte(err.Error())
		return -int32(len(out))
	}
	return 0
}

//go:wasmexport doom_tics
func doomTics(n int32) {
	for i := int32(0); i < n; i++ {
		b := in[i*TicCmdSize:]
		gore.StepTic(ticCmd(b))
	}
}

func ticCmd(b []byte) gore.TicCmd {
	return gore.TicCmd{
		Forward:   int8(b[0]),
		Side:      int8(b[1]),
		AngleTurn: int16(binary.LittleEndian.Uint16(b[2:])),
		Buttons:   b[4],
	}
}

//go:wasmexport doom_net_tics
func doomNetTics(n int32) {
	for i := int32(0); i < n; i++ {
		b := in[i*NetTicSize:]
		t := gore.NetTic{Leave: b[0], Join: b[1]}
		for p := range t.Cmds {
			t.Cmds[p] = ticCmd(b[4+p*TicCmdSize:])
		}
		gore.StepNetTic(&t)
	}
}

// doom_view chooses whose view doom_render draws.
//
//go:wasmexport doom_view
func doomView(p int32) {
	gore.StepSetView(p)
}

//go:wasmexport doom_players
func doomPlayers() int32 {
	return int32(gore.PlayersInGame())
}

// doom_render draws a frame; output is 320*200 palette indices followed by
// the 768 byte RGB palette.
//
//go:wasmexport doom_render
func doomRender() int32 {
	gore.StepRender()
	out = append(append(out[:0], gore.FrameBuffer()...), gore.Palette()...)
	return int32(len(out))
}

// doom_stats outputs gore.Stats as JSON, from the point of view of the
// console player.
//
//go:wasmexport doom_stats
func doomStats() int32 {
	out, _ = json.Marshal(gore.GetStats())
	return int32(len(out))
}

// doom_player_stats outputs gore.Stats as JSON, from player p's point of view.
//
//go:wasmexport doom_player_stats
func doomPlayerStats(p int32) int32 {
	out, _ = json.Marshal(gore.GetPlayerStats(p))
	return int32(len(out))
}

//go:wasmexport doom_checksum
func doomChecksum() int32 {
	return int32(gore.Checksum())
}

//go:wasmexport doom_can_save
func doomCanSave() int32 {
	if gore.CanSave() {
		return 1
	}
	return 0
}

// doom_save outputs a savegame. On failure it outputs an error message and
// returns its negated length.
//
//go:wasmexport doom_save
func doomSave() int32 {
	data, err := gore.SaveGameBytes()
	if err != nil {
		out = []byte(err.Error())
		return -int32(len(out))
	}
	out = data
	return int32(len(out))
}

// doom_sounds outputs the sound events since the last call as JSON, or
// returns 0 if there are none (the common case).
//
//go:wasmexport doom_sounds
func doomSounds() int32 {
	ev := gore.StepSoundEvents()
	if len(ev) == 0 {
		return 0
	}
	out, _ = json.Marshal(ev)
	return int32(len(out))
}

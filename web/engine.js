// engine.js runs build/doom.wasm in the browser. It's the very same module
// the worker runs under wazero, so given the same snapshot and ticcmds both
// produce bit-for-bit identical games.
//
// The module is a Go wasip1 "reactor", so it needs a handful of WASI
// imports. Like the worker's sandbox, this shim gives it no filesystem, a
// fake clock and a constant "random" source: nothing but ticcmds can
// influence the game.

const TIC_CMD_SIZE = 8;
const MAX_PLAYERS = 4;
const NET_TIC_SIZE = 4 + MAX_PLAYERS * TIC_CMD_SIZE; // leave mask, join mask, padding, then one ticcmd per player
const ERRNO_SUCCESS = 0, ERRNO_BADF = 8, ERRNO_NOSYS = 52;

function wasiImports(getMemory, log) {
  let fakeNanos = 0n;
  const view = () => new DataView(getMemory().buffer);
  const bytes = () => new Uint8Array(getMemory().buffer);
  let line = "";
  const impl = {
    args_sizes_get(argc, bufSize) { view().setUint32(argc, 0, true); view().setUint32(bufSize, 0, true); return 0; },
    args_get() { return 0; },
    environ_sizes_get(count, bufSize) { view().setUint32(count, 0, true); view().setUint32(bufSize, 0, true); return 0; },
    environ_get() { return 0; },
    clock_res_get(id, out) { view().setBigUint64(out, 1n, true); return 0; },
    clock_time_get(id, precision, out) {
      // Same as the worker: advances 1ms per reading from DOOM's release date.
      fakeNanos += 1000000n;
      const t = id === 0 ? 755481600n * 1000000000n + fakeNanos : fakeNanos;
      view().setBigUint64(out, t, true);
      return 0;
    },
    random_get(ptr, len) { bytes().fill(0, ptr, ptr + len); return 0; },
    fd_write(fd, iovs, iovsLen, nwritten) {
      const v = view(); let n = 0;
      for (let i = 0; i < iovsLen; i++) {
        const p = v.getUint32(iovs + i * 8, true), l = v.getUint32(iovs + i * 8 + 4, true);
        line += new TextDecoder().decode(bytes().subarray(p, p + l));
        n += l;
      }
      let nl;
      while ((nl = line.indexOf("\n")) >= 0) { log(line.slice(0, nl)); line = line.slice(nl + 1); }
      v.setUint32(nwritten, n, true);
      return 0;
    },
    fd_fdstat_get(fd, out) { bytes().fill(0, out, out + 24); view().setUint8(out, 2); return fd <= 2 ? 0 : ERRNO_BADF; },
    fd_fdstat_set_flags() { return 0; },
    fd_prestat_get() { return ERRNO_BADF; }, // no preopened directories: no filesystem
    fd_prestat_dir_name() { return ERRNO_BADF; },
    fd_close() { return 0; },
    poll_oneoff(inPtr, outPtr, nsubs, nevents) {
      // Go uses this to sleep. Time is fake, so wake every subscription now.
      const v = view();
      for (let i = 0; i < nsubs; i++) {
        const userdata = v.getBigUint64(inPtr + i * 48, true);
        const tag = v.getUint8(inPtr + i * 48 + 8);
        bytes().fill(0, outPtr + i * 32, outPtr + i * 32 + 32);
        v.setBigUint64(outPtr + i * 32, userdata, true);
        v.setUint8(outPtr + i * 32 + 10, tag);
      }
      v.setUint32(nevents, nsubs, true);
      return 0;
    },
    sched_yield() { return 0; },
    proc_exit(code) { throw new Error(`DOOM engine exited with code ${code}`); },
  };
  // Anything else (path_open, fd_read, sockets...) is simply unsupported.
  return new Proxy(impl, { get: (t, k) => t[k] || (() => ERRNO_NOSYS) });
}

let modulePromise, wadPromise;

// useSources overrides where the engine and WAD come from (for tests).
export function useSources(module, wad) {
  modulePromise = Promise.resolve(module);
  wadPromise = Promise.resolve(wad);
}

export function preload() {
  modulePromise ||= WebAssembly.compileStreaming(fetch("/doom.wasm"));
  wadPromise ||= fetch("/doom1.wad").then(r => r.arrayBuffer()).then(b => new Uint8Array(b));
  return Promise.all([modulePromise, wadPromise]);
}

export class Engine {
  // create boots a fresh engine: same options, same snapshot, same result.
  static async create({ skill = 3, episode = 1, map = 1, mode = "", snapshot = null, log = () => {} }) {
    const [module, wad] = await preload();
    let memory;
    const instance = await WebAssembly.instantiate(module, {
      wasi_snapshot_preview1: wasiImports(() => memory, log),
    });
    memory = instance.exports.memory;
    instance.exports._initialize();
    const e = new Engine(instance);
    e.input(wad);
    e.x.doom_wad(wad.length);
    // Must match doomvm.Options.Args, or the two engines play different games.
    const argv = ["-skill", skill, "-warp", episode, map];
    if (mode === "coop") argv.push("-solo-net");
    if (mode === "deathmatch") argv.push("-solo-net", "-deathmatch");
    const args = new TextEncoder().encode(argv.join("\0"));
    e.input(args);
    e.x.doom_init(args.length);
    if (snapshot && snapshot.length) {
      e.input(snapshot);
      const n = e.x.doom_load(snapshot.length);
      if (n < 0) throw new Error("loading snapshot: " + e.outputText(-n));
    }
    return e;
  }

  constructor(instance) {
    this.x = instance.exports;
    this.rgba = typeof ImageData !== "undefined" ? new ImageData(320, 200) : { data: new Uint8ClampedArray(320 * 200 * 4) };
  }

  get mem() { return new Uint8Array(this.x.memory.buffer); }

  input(data) {
    const p = this.x.doom_alloc(data.length);
    this.mem.set(data, p);
  }

  output(n) { const p = this.x.doom_out(); return this.mem.slice(p, p + n); }
  outputText(n) { return new TextDecoder().decode(this.output(n)); }

  // tics runs one tic per command ({f, s, t, b} like the Go TicCmd).
  tics(cmds) {
    if (!cmds.length) return;
    const buf = new Uint8Array(cmds.length * TIC_CMD_SIZE);
    const v = new DataView(buf.buffer);
    cmds.forEach((c, i) => {
      const o = i * TIC_CMD_SIZE;
      v.setInt8(o, c.f || 0);
      v.setInt8(o + 1, c.s || 0);
      v.setInt16(o + 2, c.t || 0, true);
      v.setUint8(o + 4, c.b || 0);
    });
    this.input(buf);
    this.x.doom_tics(cmds.length);
  }

  // netTics runs one multiplayer tic per entry ({c: [cmd per player], l, j}
  // like the Go NetTic).
  netTics(tics) {
    if (!tics.length) return;
    const buf = new Uint8Array(tics.length * NET_TIC_SIZE);
    const v = new DataView(buf.buffer);
    tics.forEach((t, i) => {
      const o = i * NET_TIC_SIZE;
      v.setUint8(o, t.l || 0);
      v.setUint8(o + 1, t.j || 0);
      (t.c || []).slice(0, MAX_PLAYERS).forEach((c, p) => {
        const q = o + 4 + p * TIC_CMD_SIZE;
        v.setInt8(q, c.f || 0);
        v.setInt8(q + 1, c.s || 0);
        v.setInt16(q + 2, c.t || 0, true);
        v.setUint8(q + 4, c.b || 0);
      });
    });
    this.input(buf);
    this.x.doom_net_tics(tics.length);
  }

  // view chooses whose eyes render() looks through (multiplayer).
  view(player) { this.x.doom_view(player); }

  checksum() { return this.x.doom_checksum() >>> 0; }

  // sounds returns the sound events since the last call (see audio.js).
  sounds() {
    const n = this.x.doom_sounds();
    return n ? JSON.parse(this.outputText(n)) : [];
  }

  stats() { return JSON.parse(this.outputText(this.x.doom_stats())); }

  // render returns an ImageData of the current frame.
  render() {
    const n = this.x.doom_render();
    const p = this.x.doom_out();
    const mem = this.mem;
    const pix = mem.subarray(p, p + 64000), pal = mem.subarray(p + 64000, p + n);
    const out = this.rgba.data;
    for (let i = 0, o = 0; i < 64000; i++, o += 4) {
      const c = pix[i] * 3;
      out[o] = pal[c]; out[o + 1] = pal[c + 1]; out[o + 2] = pal[c + 2]; out[o + 3] = 255;
    }
    return this.rgba;
  }
}

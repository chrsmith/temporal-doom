// audio.js plays DOOM's sound effects with Web Audio.
//
// The engine doesn't produce audio. It produces sound *events* (start this
// effect on channel 3 at this volume and stereo separation, move it, stop it),
// exactly as vanilla DOOM hands them to its sound driver. The samples come
// straight from the WAD, which the page has already downloaded for the engine.

export class Sound {
  constructor() {
    this.ctx = null;
    this.lumps = new Map();    // lump name -> [offset, size]
    this.wad = null;
    this.buffers = new Map();  // effect name -> AudioBuffer (or null if missing)
    this.channels = new Map(); // DOOM channel -> { src, gain, pan }
    this.enabled = true;
    this.volume = 0.8;
  }

  // setWAD indexes the WAD's lump directory.
  setWAD(wad) {
    const v = new DataView(wad.buffer, wad.byteOffset, wad.byteLength);
    const count = v.getInt32(4, true), dir = v.getInt32(8, true);
    for (let i = 0; i < count; i++) {
      const e = dir + i * 16;
      const name = String.fromCharCode(...wad.subarray(e + 8, e + 16)).replace(/\0.*$/, "").toUpperCase();
      this.lumps.set(name, [v.getInt32(e, true), v.getInt32(e + 4, true)]);
    }
    this.wad = wad;
  }

  // unlock creates or resumes the AudioContext. Browsers only allow that
  // from a user gesture, so call it from click handlers.
  unlock() {
    if (!this.ctx) {
      this.ctx = new AudioContext();
      this.master = this.ctx.createGain();
      this.master.gain.value = this.volume;
      this.master.connect(this.ctx.destination);
    }
    if (this.ctx.state === "suspended") this.ctx.resume();
  }

  setVolume(v) {
    this.volume = v;
    if (this.master) this.master.gain.value = v;
  }

  setEnabled(on) {
    this.enabled = on;
    if (!on) this.stopAll();
  }

  // buffer decodes a DMX sound lump: u16 format (3), u16 sample rate,
  // u32 sample count, then unsigned 8-bit samples with 16 bytes of padding
  // at each end.
  buffer(name) {
    if (this.buffers.has(name)) return this.buffers.get(name);
    let buf = null;
    const lump = this.lumps.get("DS" + name.toUpperCase());
    if (lump && this.wad) {
      const [off, size] = lump;
      const v = new DataView(this.wad.buffer, this.wad.byteOffset + off, size);
      const rate = v.getUint16(2, true), count = v.getUint32(4, true);
      const n = Math.min(count, size - 8) - 32;
      if (v.getUint16(0, true) === 3 && n > 0) {
        buf = this.ctx.createBuffer(1, n, rate);
        const out = buf.getChannelData(0);
        const samples = this.wad.subarray(off + 8 + 16, off + 8 + 16 + n);
        for (let i = 0; i < n; i++) out[i] = (samples[i] - 128) / 128;
      }
    }
    this.buffers.set(name, buf);
    return buf;
  }

  // play applies a list of engine sound events.
  play(events) {
    if (!events?.length || !this.enabled || !this.ctx || this.ctx.state !== "running") return;
    for (const ev of events) {
      if (ev.kind === "start") {
        this.stop(ev.ch); // a DOOM channel plays one sound at a time
        const buf = this.buffer(ev.name);
        if (!buf) continue;
        const src = this.ctx.createBufferSource();
        const gain = this.ctx.createGain();
        const pan = this.ctx.createStereoPanner();
        src.buffer = buf;
        src.connect(gain).connect(pan).connect(this.master);
        this.params({ gain, pan }, ev);
        const c = { src, gain, pan };
        src.onended = () => { if (this.channels.get(ev.ch) === c) this.channels.delete(ev.ch); };
        this.channels.set(ev.ch, c);
        src.start();
      } else if (ev.kind === "update") {
        const c = this.channels.get(ev.ch);
        if (c) this.params(c, ev);
      } else if (ev.kind === "stop") {
        this.stop(ev.ch);
      }
    }
  }

  // params maps DOOM's volume (0-127) and separation (0 left, 128 centre,
  // 254 right) onto Web Audio.
  params(c, ev) {
    c.gain.gain.value = (ev.vol || 0) / 127;
    c.pan.pan.value = Math.max(-1, Math.min(1, ((ev.sep ?? 128) - 128) / 128));
  }

  stop(ch) {
    const c = this.channels.get(ch);
    if (!c) return;
    this.channels.delete(ch);
    try { c.src.stop(); } catch {}
  }

  stopAll() {
    for (const ch of [...this.channels.keys()]) this.stop(ch);
  }
}

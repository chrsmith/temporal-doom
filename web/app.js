import { Engine, preload } from "/engine.js";
import { Sound } from "/audio.js";

const $ = s => document.querySelector(s);
const TICRATE = 35;
let config = { maxBatch: 105 };

// ---------------------------------------------------------------- API

// Every request says which player (tab) it's from, so the server can tell
// whether someone else is playing a game.
async function api(method, path, body) {
  const headers = { "X-Doom-Player": PLAYER_ID };
  if (body) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  if (!res.ok) {
    let msg = res.statusText, code;
    try { ({ error: msg, code } = await res.json()); } catch {}
    const err = new Error(msg);
    err.status = res.status;
    err.code = code; // "busy": someone else is playing the game
    throw err;
  }
  return res.json();
}

const gamePath = (id, rest) => `/api/games/${encodeURIComponent(id)}/${rest}`;

const b64decode = s => s ? Uint8Array.from(atob(s), c => c.charCodeAt(0)) : null;
const sleep = ms => new Promise(r => setTimeout(r, ms));

// Links into the Temporal UI: the local dev server's when the site is served
// from this machine, Temporal Cloud's (from the server's config) otherwise.
const LOCAL = ["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
const temporalUI = path => {
  const base = LOCAL ? "http://localhost:8233" : (config.temporalUI || "https://cloud.temporal.io");
  return `${base}/namespaces/${encodeURIComponent(config.namespace)}${path}`;
};

// Who you are in multiplayer games. Per tab, so a refresh keeps your marine
// and a second tab is a second player.
const PLAYER_ID = sessionStorage.doomPlayer ||= crypto.randomUUID();

// The New Game dialog suggests one of these. They must be valid player names
// (letters, digits, _ and -, at most 24 characters; see game.PlayerName).
const NAMES = ["Player1", "YOLO", "Ziggy", "DurableWorkflow", "ContinueAsNew", "RetryPolicy", "Heartbeat",
  "Idempotent", "TaskQueue", "SignalBoost", "ReplayMe", "Doomguy", "SpaceCadet", "Rookie", "NightOwl",
  "PixelPusher", "CoffeeBreak", "LateForWork", "Captain_Pancake", "Noodle", "BFG-Enthusiast", "IDDQD"];
const randomName = () => NAMES[Math.floor(Math.random() * NAMES.length)];
// The name this tab joins multiplayer games as: the one it last started a
// game with, else a random one.
const playerName = () => sessionStorage.doomName ||= randomName();

// Why you're spectating instead of playing.
const SOMEONE_PLAYING = "Someone is playing this game, so you're spectating.";
const GAME_FULL = "This game is full, so you're spectating.";
const notice = msg => { $("#notice").textContent = msg; $("#notice").hidden = !msg; };

// ---------------------------------------------------------------- lobby

async function refreshLobby() {
  const q = new FormData($("#search")).get("q") || "";
  try {
    const { games } = await api("GET", "/api/games" + (q ? "?q=" + encodeURIComponent(q) : ""));
    $("#games tbody").innerHTML = games.map(g => {
      const n = (g.players || []).length;
      const closed = g.busy ? "Someone is playing" : g.mode && n >= 4 ? "Game is full" : "";
      const join = closed ? `<span class="closed">${closed}</span>` : `<a href="#/game/${encodeURIComponent(g.id)}">join</a>`;
      return `
      <tr><td>${esc(g.player)}</td><td>${g.mode ? `${esc(g.mode)} (${n}/4)` : "single"}</td><td>${esc(g.map)}</td><td>${g.health}</td><td>${g.kills}</td><td>${g.deaths}</td>
      <td>${join} &middot; <a href="#/game/${encodeURIComponent(g.id)}?watch">watch</a></td></tr>`;
    }).join("")
      || `<tr><td colspan="7" class="help">No games running.</td></tr>`;
  } catch (e) {
    $("#games tbody").innerHTML = `<tr><td colspan="7" class="bad">${esc(e.message)}</td></tr>`;
  }
  const board = await api("GET", "/api/leaderboard").catch(() => []);
  $("#leaderboard tbody").innerHTML = board.map(r => `
    <tr><td>${esc(r.map)}</td><td>${esc(r.player)}</td><td>${fmtTime(r.timeTics)}</td>
    <td>${r.kills}/${r.totalKills}</td><td>${r.items}/${r.totalItems}</td><td>${r.secrets}/${r.totalSecrets}</td></tr>`).join("")
    || `<tr><td colspan="6" class="help">Finish a level to get on the board.</td></tr>`;
}

const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const fmtTime = tics => { const s = Math.floor(tics / TICRATE); return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; };

const newGameDialog = $("#new-game-dialog");
document.querySelectorAll("[data-new-game]").forEach(b => b.addEventListener("click", () => {
  $("#new-game-error").hidden = true;
  $("#name").value = randomName();
  newGameDialog.showModal();
  $("#name").focus();
}));
newGameDialog.querySelectorAll("[data-close]").forEach(b => b.addEventListener("click", () => newGameDialog.close()));
// Clicking the backdrop (outside the form) closes the dialog too.
newGameDialog.addEventListener("click", ev => { if (ev.target === newGameDialog) newGameDialog.close(); });

$("#new-game").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  const submit = ev.target.querySelector("[type=submit]");
  submit.disabled = true;
  try {
    const player = f.get("player") || "marine";
    const { id } = await api("POST", "/api/games", {
      player, skill: +f.get("skill"), episode: 1, map: +f.get("map"), mode: f.get("game-mode"),
    });
    sessionStorage.doomName = player;
    newGameDialog.close();
    location.hash = `#/game/${encodeURIComponent(id)}`;
  } catch (e) {
    $("#new-game-error").textContent = `Couldn't start the game: ${e.message}`;
    $("#new-game-error").hidden = false;
  } finally {
    submit.disabled = false;
  }
});
$("#search").addEventListener("submit", ev => { ev.preventDefault(); refreshLobby(); });

// ---------------------------------------------------------------- input

const keys = new Set();
let mouseDX = 0, mouseFire = false, weapon = 0;
const GAME_KEYS = new Set(["KeyW", "KeyA", "KeyS", "KeyD", "KeyE", "KeyF", "Space", "ArrowUp", "ArrowDown",
  "ArrowLeft", "ArrowRight", "ControlLeft", "ControlRight", "AltLeft", "AltRight", "ShiftLeft", "ShiftRight"]);

addEventListener("keydown", ev => {
  if (!session?.playing) return;
  // Esc pauses. With the pointer locked, the browser also releases it.
  if (ev.code === "Escape") { session.setPlaying(false); return; }
  if (GAME_KEYS.has(ev.code)) ev.preventDefault();
  if (/^Digit[1-7]$/.test(ev.code)) weapon = +ev.code.slice(5);
  keys.add(ev.code);
});
addEventListener("keyup", ev => keys.delete(ev.code));
addEventListener("blur", () => keys.clear());
addEventListener("mousemove", ev => { if (document.pointerLockElement) mouseDX += ev.movementX; });
addEventListener("mousedown", ev => { if (document.pointerLockElement && ev.button === 0) mouseFire = true; });
addEventListener("mouseup", ev => { if (ev.button === 0) mouseFire = false; });

const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

// buildCmd samples the keyboard and mouse into one ticcmd, like DOOM's
// G_BuildTiccmd. Always-run is on; hold Shift to walk.
function buildCmd() {
  const run = !(keys.has("ShiftLeft") || keys.has("ShiftRight")) ? 1 : 0;
  const fwd = [0x19, 0x32][run], side = [0x18, 0x28][run], turn = [640, 1280][run];
  const strafe = keys.has("AltLeft") || keys.has("AltRight");
  let f = 0, s = 0, t = 0, b = 0;
  if (keys.has("KeyW") || keys.has("ArrowUp")) f += fwd;
  if (keys.has("KeyS") || keys.has("ArrowDown")) f -= fwd;
  if (keys.has("KeyA")) s -= side;
  if (keys.has("KeyD")) s += side;
  if (keys.has("ArrowLeft")) strafe ? (s -= side) : (t += turn);
  if (keys.has("ArrowRight")) strafe ? (s += side) : (t -= turn);
  t -= mouseDX * 16;
  mouseDX = 0;
  if (mouseFire || keys.has("ControlLeft") || keys.has("ControlRight") || keys.has("KeyF")) b |= 1;
  if (keys.has("KeyE") || keys.has("Space")) b |= 2;
  if (weapon) { b |= 4 | ((weapon - 1) << 3); weapon = 0; }
  const cmd = {};
  if ((f = clamp(f, -0x32, 0x32))) cmd.f = f;
  if ((s = clamp(s, -0x32, 0x32))) cmd.s = s;
  if ((t = clamp(Math.round(t), -32768, 32767))) cmd.t = t;
  if (b) cmd.b = b;
  return cmd;
}

// ---------------------------------------------------------------- game session

const sound = new Sound();
$("#sound-on").addEventListener("change", ev => { sound.unlock(); sound.setEnabled(ev.target.checked); });
$("#volume").addEventListener("input", ev => { sound.unlock(); sound.setVolume(ev.target.value / 100); });
addEventListener("pointerdown", () => sound.unlock()); // browsers only allow audio after a gesture

const canvas = $("#screen");
const ctx2d = canvas.getContext("2d");
let session = null;

const MODE_HELP = {
  lockstep: "Your browser runs the same doom.wasm as the worker and renders at full speed. Every batch the workflow applies is checked against your local checksum, the way DOOM's 1993 IPX netcode checked consistency.",
  thin: "The browser only sends input. Each frame is one Update (apply ticcmds) followed by one Query (fetch the frame the workflow rendered).",
  watch: "Read-only: polls the frame Query. Open a second tab to watch yourself play.",
};

class Session {
  constructor(id, mode) {
    this.id = id;
    this.mode = mode;
    this.playing = false;
    this.pending = [];        // ticcmds not yet sent
    this.inflight = false;
    this.ackedTic = 0;        // server's tic
    this.localTic = 0;        // lockstep: local engine's tic
    this.segment = -1;
    this.engine = null;
    this.checksums = new Map(); // lockstep: tic -> local checksum
    this.rtts = [];
    this.batches = [];
    this.frames = 0;
    this.resyncs = 0;
    this.clock = 0;           // input clock origin (ms)
    this.made = 0;            // tics generated since clock origin
    this.stopped = false;
    this.lastStats = null;
    this.lastSend = 0;
    this.soundTic = -1;       // thin/watch: last tic we've played sounds for
  }

  log(msg) {
    const el = $("#log");
    el.textContent += `${new Date().toLocaleTimeString()} ${msg}\n`;
    el.scrollTop = el.scrollHeight;
  }

  async start() {
    $("#wf-link").textContent = this.id;
    $("#wf-link").href = temporalUI(`/workflows/${encodeURIComponent(this.id)}`);
    await this.setMode(this.mode);
    requestAnimationFrame(t => this.frame(t));
    this.hudTimer = setInterval(() => this.hud(), 250);
  }

  stop() {
    this.stopped = true;
    clearInterval(this.hudTimer);
    this.playing = false;
    if (document.pointerLockElement) document.exitPointerLock();
  }

  async setMode(mode) {
    this.mode = mode;
    this.soundTic = -1; // don't replay sounds from before the switch
    $("#mode-help").textContent = MODE_HELP[mode];
    document.querySelector(`input[name=mode][value=${mode}]`).checked = true;
    $("#interval").value = mode === "lockstep" ? "250" : "0";
    $("#net").hidden = true;
    $("#interval-row").hidden = false;
    $("#l-rtt").textContent = "Update RTT";
    $("#l-batch").textContent = "Tics / update";
    $("#h-waiting").textContent = "-";
    if (mode === "watch") { this.playing = false; this.pending = []; }
    $("#overlay").hidden = mode === "watch" || this.playing;
    if (mode === "watch") { this.watch(); return; }
    if (mode === "lockstep") {
      this.inflight = true;
      try { await this.resync("joining"); } finally { this.inflight = false; }
    } else {
      const st = await api("GET", gamePath(this.id, "state"));
      this.ackedTic = st.stats.tic;
      this.segment = st.segment;
      this.engine = null;
      await this.fetchFrame();
    }
  }

  // resync makes the local engine identical to the workflow's: boot a fresh
  // engine, load this run's snapshot, and replay every ticcmd since. This is
  // exactly what a Temporal worker does when it replays the run's history.
  async resync(reason) {
    const t0 = performance.now();
    const sync = await api("GET", gamePath(this.id, "sync"));
    if (sync.engineVersion !== config.engineVersion) {
      this.log(`engine version mismatch (${sync.engineVersion} vs ${config.engineVersion}); lockstep impossible`);
      return this.setMode("thin");
    }
    sync.tics ||= [];
    const e = await Engine.create({ ...sync.config, snapshot: b64decode(sync.snapshot) });
    e.tics(sync.tics);
    if (e.checksum() !== sync.checksum >>> 0) {
      this.log(`browser engine disagrees with the worker at tic ${sync.tic}!`);
    }
    this.engine = e;
    this.ackedTic = sync.tic;
    this.segment = sync.segment;
    // Re-predict input we haven't sent yet on top of the authoritative state.
    this.checksums.clear();
    this.localTic = sync.tic;
    for (const cmd of this.pending) {
      e.tics([cmd]);
      this.checksums.set(++this.localTic, e.checksum());
    }
    e.sounds(); // catching up replays tics that have already been heard
    this.draw(e.render());
    this.log(`synced (${reason}): segment ${sync.segment}, snapshot ${sync.snapshot ? Math.round(sync.snapshot.length * 0.75 / 1024) + "KB" : "none"}, ` +
      `replayed ${sync.tics.length} tics in ${Math.round(performance.now() - t0)}ms`);
  }

  setPlaying(p) {
    this.playing = p && this.mode !== "watch";
    $("#overlay").hidden = this.playing || this.mode === "watch";
    $("#overlay").textContent = "Paused - click to play";
    this.clock = performance.now();
    this.made = 0;
    if (!this.playing) {
      keys.clear();
      // Release the mouse, or clicks keep going to the canvas instead of
      // the overlay.
      if (document.pointerLockElement) document.exitPointerLock();
    }
  }

  frame(now) {
    if (this.stopped) return;
    requestAnimationFrame(t => this.frame(t));
    if (this.mode === "watch") return;

    // The client is DOOM's clock: 35 ticcmds per second while playing.
    // When paused, no ticcmds are sent and the game simply doesn't advance.
    if (this.playing) {
      let due = Math.floor((now - this.clock) * TICRATE / 1000) - this.made;
      if (due > TICRATE) { this.clock = now; this.made = 0; due = 1; } // tab was asleep
      for (let i = 0; i < due; i++) {
        const cmd = buildCmd();
        this.made++;
        this.pending.push(cmd);
        if (this.mode === "lockstep" && this.engine) {
          this.engine.tics([cmd]);
          this.checksums.set(++this.localTic, this.engine.checksum());
        }
      }
      if (due > 0 && this.mode === "lockstep" && this.engine) {
        this.draw(this.engine.render());
        sound.play(this.engine.sounds());
      }
    }
    // Lockstep doesn't need low latency (the browser predicts locally), so
    // it batches: every update and every query is a billable Temporal Cloud
    // Action, and 35 updates/s is ~126k Actions per hour of DOOM.
    const interval = +$("#interval").value || 0;
    if (!this.inflight && this.pending.length && now - this.lastSend >= interval) {
      this.lastSend = now;
      this.send();
    }
  }

  // send ships everything that has accumulated since the last update was
  // acknowledged. Only one update is ever in flight, so the batch size adapts
  // to the round-trip time: a slow network means bigger batches, not a
  // slower game.
  async send() {
    this.inflight = true;
    const cmds = this.pending.splice(0, config.maxBatch);
    const startTic = this.ackedTic;
    const t0 = performance.now();
    try {
      const res = await api("POST", gamePath(this.id, "tics"), { startTic, cmds });
      this.rtts.push(performance.now() - t0);
      this.batches.push(cmds.length);
      this.ackedTic = res.stats.tic;
      this.lastStats = res.stats;
      if (this.mode === "lockstep" && this.engine) {
        const local = this.checksums.get(this.ackedTic);
        if (res.segment !== this.segment) {
          this.log(`workflow continued-as-new: now in segment ${res.segment}`);
          await this.resync("new run");
        } else if (local !== res.stats.checksum >>> 0) {
          this.resyncs++;
          this.log(`DESYNC at tic ${this.ackedTic}: local ${local?.toString(16)} vs workflow ${(res.stats.checksum >>> 0).toString(16)}`);
          await this.resync("desync");
        } else {
          $("#h-sync").innerHTML = `<span class="ok">in sync @ ${this.ackedTic}</span>`;
        }
        for (const t of this.checksums.keys()) if (t < this.ackedTic) this.checksums.delete(t);
      } else if (this.mode === "thin") {
        await this.fetchFrame();
      }
    } catch (e) {
      this.log(`update rejected: ${e.message}`);
      if (e.code === "busy") {
        // Someone else got here first. Watch them instead.
        this.setPlaying(false);
        notice(SOMEONE_PLAYING);
        await this.setMode("watch");
        return;
      }
      if (e.status === 409 || e.status === 500) {
        // Rejected by the validator (e.g. the game was Reset to an earlier
        // point) or the game ended. Rebuild from the workflow's state.
        this.resyncs++;
        if (this.mode === "lockstep") await this.resync("rejected").catch(err => this.log(err.message));
        else this.ackedTic = (await api("GET", gamePath(this.id, "state"))).stats.tic;
      }
      await new Promise(r => setTimeout(r, 250));
    } finally {
      this.inflight = false;
    }
  }

  async fetchFrame() {
    // The frame Query also returns sound events newer than the last frame
    // we saw, so thin clients and spectators get sound without an engine.
    const res = await fetch(gamePath(this.id, "frame") + `?since=${Math.max(0, this.soundTic)}&view=${this.view || 0}`, { cache: "no-store" });
    if (!res.ok) throw new Error(`frame: ${res.status}`);
    const stats = res.headers.get("X-Doom-Stats");
    if (stats) this.lastStats = JSON.parse(stats);
    const sounds = res.headers.get("X-Doom-Sounds");
    if (sounds && this.soundTic >= 0) sound.play(JSON.parse(sounds));
    if (this.lastStats) this.soundTic = this.lastStats.tic;
    const bmp = await createImageBitmap(await res.blob());
    ctx2d.drawImage(bmp, 0, 0);
    this.frames++;
  }

  async watch() {
    while (!this.stopped && this.mode === "watch") {
      try {
        await this.fetchFrame();
        this.ackedTic = this.lastStats?.tic ?? this.ackedTic;
      } catch (e) { this.log(e.message); await new Promise(r => setTimeout(r, 1000)); }
      await new Promise(r => setTimeout(r, 50));
    }
  }

  draw(img) {
    ctx2d.putImageData(img, 0, 0);
    this.frames++;
  }

  hud() {
    const avg = a => a.length ? a.reduce((x, y) => x + y, 0) / a.length : 0;
    const rtts = this.rtts.splice(0), batches = this.batches.splice(0);
    const fps = this.frames * 4; this.frames = 0;
    $("#h-segment").textContent = this.segment;
    $("#h-tic").textContent = this.mode === "lockstep" ? this.localTic : this.ackedTic + this.pending.length;
    $("#h-acked").textContent = this.ackedTic;
    if (rtts.length) $("#h-rtt").textContent = `${avg(rtts).toFixed(1)} ms`;
    if (batches.length) $("#h-batch").textContent = avg(batches).toFixed(1);
    $("#h-fps").textContent = fps;
    $("#h-resyncs").textContent = this.resyncs;
    if (this.mode !== "lockstep") $("#h-sync").textContent = "-";
  }
}

// ---------------------------------------------------------------- multiplayer

// MAX_AHEAD is how much input (in tics) we make before the game has
// confirmed it. If another player falls behind, we stop and wait for them,
// as DOOM did in 1993.
const MAX_AHEAD = 20;
// PLAYOUT_SLACK is how many confirmed tics may pile up before we play them
// faster than real time.
const PLAYOUT_SLACK = 4;
const PING_MS = 1000;
const PLAYER_COLORS = ["#5c5", "#77e", "#b85", "#e44"]; // DOOM's green, indigo, brown and red

const NET_HELP = {
  lockstep: "Multiplayer lockstep, the 1993 way, with the web server as the arbiter. Your browser streams input to the server over a WebSocket; the server merges everyone's, sends each tic back the moment it's complete, and your browser runs it. About once a second the server records what it has played in the workflow, whose checksums prove every engine agrees. Esc pauses: your marine stands still and the game goes on.",
  thin: "Multiplayer thin client: you send input to the server, and fetch the frame the workflow rendered from your marine's view. The workflow only hears about the game once a second, so expect a second of lag.",
  watch: "Spectate any player through the frame Query (about a second behind the live game). Your marine (if you have one) stands still while you watch.",
};

class NetSession extends Session {
  constructor(id, mode, state) {
    super(id, mode);
    this.slot = -1;            // our player slot, if we have one
    this.view = 0;             // whose view the frame Query renders
    this.seated = false;       // the server is waiting for our input
    this.joining = false;      // a join is on its way to the server
    this.next = 0;             // the tic our next ticcmd will run at
    this.synced = false;       // following the game: we have every tic so far
    this.received = [];        // tics we haven't played yet
    this.sums = new Map();     // tic -> our engine's checksum (recent tics)
    this.expect = new Map();   // tic -> the workflow's checksum, for tics we haven't played yet
    this.lastPlay = 0;
    this.lastPing = 0;
    this.waiting = 0;          // bitmask of the players the game is waiting for
    this.players = [];         // from the server: slot, name, status
    this.stats = new Map();    // slot -> health, kills, deaths, from the state Query
    this.mergeStats(state.players);
    this.inbox = Promise.resolve();
    this.retries = 0;
    this.over = false;
  }

  async start() {
    this.connect();
    this.playersTimer = setInterval(() => this.refreshPlayers(), 2000);
    await super.start();
  }

  stop() {
    super.stop();
    clearInterval(this.playersTimer);
    $("#banner").hidden = true;
    // Leaving for the lobby gives up the slot. (Closing the tab just
    // closes the socket, which the server treats as a pause, so a reload
    // gets the same marine back.)
    if (this.slot >= 0) this.post({ t: "leave" });
    this.ws?.close();
  }

  // connect opens the WebSocket to the server's room for this game. The
  // room answers with a sync message, then streams tics.
  connect() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(`${proto}://${location.host}/api/games/${encodeURIComponent(this.id)}/net?player=${encodeURIComponent(PLAYER_ID)}`);
    this.ws = ws;
    ws.onopen = () => { this.retries = 0; };
    // Messages are handled one at a time, in order, even though handling
    // a sync has to wait for a fresh engine.
    ws.onmessage = ev => {
      const m = JSON.parse(ev.data);
      this.inbox = this.inbox.then(() => this.handle(m)).catch(e => this.log(e.message));
    };
    ws.onclose = () => {
      if (this.ws !== ws || this.stopped) return;
      this.synced = this.seated = this.joining = false;
      if (this.over) return;
      const delay = Math.min(5000, 250 * 2 ** this.retries++);
      this.log(`disconnected from the server; reconnecting in ${delay}ms`);
      setTimeout(() => { if (!this.stopped) this.connect(); }, delay);
    };
  }

  post(m) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
  }

  resync(reason) {
    this.log(`resyncing (${reason})`);
    this.synced = false;
    this.post({ t: "resync" });
  }

  async handle(m) {
    switch (m.t) {
    case "sync": return this.onSync(m);
    case "tics": {
      if (!this.synced) return;
      const end = m.from + m.tics.length;
      if (!this.engine) { this.ackedTic = end; return; } // thin client or spectating
      if (end <= this.ackedTic) return; // already have them
      if (m.from > this.ackedTic) return this.resync(`missed tics ${this.ackedTic}-${m.from}`);
      if (this.engine) this.received.push(...m.tics.slice(this.ackedTic - m.from));
      this.ackedTic = end;
      return;
    }
    case "check":
      if (!this.synced) return;
      if (m.segment !== this.segment) {
        // The workflow continued-as-new from a savegame. A game resumed
        // from a savegame doesn't play out exactly like one that never
        // stopped, so pick up the new run.
        this.log(`workflow continued-as-new: now in segment ${m.segment}`);
        return this.resync("new run");
      }
      if (!this.engine) return;
      if (this.sums.has(m.tic)) this.check(m.tic, m.checksum >>> 0);
      else if (m.tic > this.localTic) this.expect.set(m.tic, m.checksum >>> 0);
      return;
    case "seat":
      if (this.joining && !m.playing) return; // from before our join
      if (m.playing && (!this.seated || m.slot !== this.slot)) this.log(`playing as player ${m.slot + 1} from tic ${m.tic}`);
      this.joining = false;
      this.slot = m.slot;
      this.seated = m.playing;
      this.next = m.tic;
      this.pending = [];
      if (m.slot >= 0) { this.view = m.slot; this.engine?.view(m.slot); }
      if (!m.playing && this.playing) {
        this.log("the game went on without you (paused)");
        this.setPlaying(false);
      }
      return this.renderPlayers();
    case "roster":
      this.players = m.players;
      this.waiting = m.waiting;
      return this.renderPlayers();
    case "pong":
      this.rtts.push(performance.now() - m.ts);
      return;
    case "error":
      this.log(`server: ${m.error}`);
      if (this.joining) { this.joining = false; this.setPlaying(false); }
      if (m.code === "full") {
        notice(GAME_FULL);
        this.setMode("watch");
      }
      return;
    case "over":
      this.over = true;
      this.log(`game over (${m.reason})`);
      this.setPlaying(false);
      $("#overlay").textContent = "Game over";
      return;
    }
  }

  // onSync rebuilds the local engine from the workflow's snapshot and tic
  // log, exactly as a worker replays the run's history, then runs the tics
  // the server has played since its last record.
  async onSync(m) {
    const t0 = performance.now();
    this.segment = m.segment;
    this.received = [];
    this.sums.clear();
    this.expect.clear();
    const pending = m.pending || [];
    if (this.mode === "lockstep" && m.engineVersion !== config.engineVersion) {
      this.log(`engine version mismatch (${m.engineVersion} vs ${config.engineVersion}); lockstep impossible`);
      this.mode = "thin";
      this.setMode("thin");
    }
    if (this.mode === "lockstep") {
      const net = m.net || [];
      const e = await Engine.create({ ...m.config, snapshot: b64decode(m.snapshot) });
      e.view(Math.max(this.slot, 0));
      e.netTics(net);
      if (e.checksum() !== m.checksum >>> 0) this.log(`browser engine disagrees with the worker at tic ${m.tic}!`);
      e.netTics(pending);
      e.sounds(); // catching up replays tics that have already been heard
      this.engine = e;
      this.draw(e.render());
      this.log(`synced: segment ${m.segment}, snapshot ${m.snapshot ? Math.round(m.snapshot.length * 0.75 / 1024) + "KB" : "none"}, ` +
        `replayed ${net.length} recorded + ${pending.length} live tics in ${Math.round(performance.now() - t0)}ms`);
    } else {
      this.engine = null;
    }
    this.localTic = this.ackedTic = m.tic + pending.length;
    this.synced = true;
    if (this.playing && !this.seated) this.join();
  }

  check(tic, want) {
    if (this.sums.get(tic) === want) {
      $("#h-sync").innerHTML = `<span class="ok">in sync @ ${tic}</span>`;
      return;
    }
    this.resyncs++;
    this.log(`DESYNC at tic ${tic}: local ${this.sums.get(tic).toString(16)} vs workflow ${want.toString(16)}`);
    this.resync("desync");
  }

  join() {
    this.joining = true;
    this.post({ t: "join", name: playerName() });
  }

  async setMode(mode) {
    const from = this.engine ? "lockstep" : this.mode;
    this.mode = mode;
    this.soundTic = -1;
    $("#mode-help").textContent = NET_HELP[mode];
    document.querySelector(`input[name=mode][value=${mode}]`).checked = true;
    $("#interval-row").hidden = true;
    $("#net").hidden = false;
    $("#view-row").hidden = mode !== "watch";
    $("#l-rtt").textContent = "Server RTT";
    $("#l-batch").textContent = "Tics / message";
    if (mode === "watch") {
      this.setPlaying(false);
      $("#overlay").hidden = true;
      this.engine = null;
      this.watch();
      return;
    }
    this.setOverlay();
    if (mode === "thin") {
      this.engine = null;
      this.pollFrames();
    } else if (from !== "lockstep" && this.ws) {
      this.resync("switched to lockstep");
    }
  }

  setOverlay() {
    $("#overlay").hidden = this.playing || this.mode === "watch";
    $("#overlay").textContent = this.over ? "Game over" : this.slot < 0 ? "Click to join" : "Paused: your marine is standing still. Click to rejoin";
  }

  setPlaying(p) {
    if (p === this.playing || (p && (this.mode === "watch" || this.over))) return;
    super.setPlaying(p);
    this.setOverlay();
    if (p) {
      if (this.synced) this.join(); // otherwise, once we are
      return;
    }
    if (this.seated) this.post({ t: "pause" });
    this.seated = this.joining = false;
    this.pending = [];
  }

  // Spectating polls the frame Query. (ackedTic stays the server's tic,
  // which is ahead of the workflow's.)
  async watch() {
    while (!this.stopped && this.mode === "watch") {
      await this.fetchFrame().catch(async e => { this.log(e.message); await sleep(1000); });
      await sleep(50);
    }
  }

  // The thin client shows the frame the workflow rendered. The workflow
  // only runs tics once the server records them, so it's about a second
  // behind.
  async pollFrames() {
    if (this.polling) return;
    this.polling = true;
    try {
      while (!this.stopped && this.mode === "thin") {
        await this.fetchFrame().catch(e => this.log(e.message));
        await sleep(100);
      }
    } finally {
      this.polling = false;
    }
  }

  frame(now) {
    if (this.stopped) return;
    requestAnimationFrame(t => this.frame(t));
    if (now - this.lastPing >= PING_MS) {
      this.lastPing = now;
      this.post({ t: "ping", ts: now });
    }
    if (this.mode === "watch") return;

    // Make input at 35 tics/s, but don't get too far ahead of the game.
    // Each ticcmd goes to the server as soon as it's made.
    if (this.playing && this.seated && this.synced) {
      let due = Math.floor((now - this.clock) * TICRATE / 1000) - this.made;
      if (due > TICRATE) { this.clock = now; this.made = 0; due = 1; } // tab was asleep
      const cmds = [];
      for (let i = 0; i < due; i++) {
        this.made++;
        if (this.next - this.ackedTic < MAX_AHEAD) { cmds.push(buildCmd()); this.next++; }
      }
      if (cmds.length) {
        this.post({ t: "cmds", cmds });
        this.batches.push(cmds.length);
      }
    }

    // Play confirmed tics at 35/s, a little faster if they're piling up.
    // This carries on while we're paused, so the game goes on behind the
    // overlay.
    if (this.engine && this.synced) {
      if (!this.received.length) this.lastPlay = now;
      const due = Math.floor((now - this.lastPlay) * TICRATE / 1000);
      const n = Math.min(this.received.length, Math.max(due, this.received.length - PLAYOUT_SLACK));
      if (n > 0) {
        this.lastPlay = n > due ? now : this.lastPlay + n * 1000 / TICRATE;
        for (const t of this.received.splice(0, n)) {
          this.engine.netTics([t]);
          const tic = ++this.localTic;
          this.sums.set(tic, this.engine.checksum());
          this.sums.delete(tic - 10 * TICRATE);
          const want = this.expect.get(tic);
          if (want !== undefined) {
            this.expect.delete(tic);
            this.check(tic, want);
            if (!this.synced) return;
          }
        }
        this.draw(this.engine.render());
        sound.play(this.engine.sounds());
      }
    }
  }

  async refreshPlayers() {
    try {
      const st = await api("GET", gamePath(this.id, "state"));
      this.mergeStats(st.players);
      this.renderPlayers();
    } catch (e) { this.log(e.message); }
  }

  // Health, kills and deaths come from the workflow (about a second
  // behind); who's playing comes from the server.
  mergeStats(players) {
    this.stats = new Map((players || []).map(p => [p.slot, p]));
  }

  renderPlayers() {
    const stat = (p, k) => this.stats.get(p.slot)?.[k] ?? "-";
    $("#players tbody").innerHTML = this.players.map(p => `
      <tr class="${p.status === "playing" ? "" : "dim"}"><td><span class="swatch" style="background:${PLAYER_COLORS[p.slot]}"></span></td>
      <td>${esc(p.name)}${p.slot === this.slot ? " (you)" : ""}</td><td>${p.status}</td><td>${stat(p, "health")}</td><td>${stat(p, "kills")}</td><td>${stat(p, "deaths")}</td></tr>`).join("");
    const sel = $("#view");
    sel.innerHTML = this.players.map(p => `<option value="${p.slot}">${esc(p.name)}</option>`).join("");
    if (!this.players.some(p => p.slot === this.view) && this.players.length) this.view = this.players[0].slot;
    sel.value = this.view;
  }

  hud() {
    super.hud();
    const others = this.waiting & ~(1 << this.slot);
    const names = this.players.filter(p => others & (1 << p.slot)).map(p => p.name);
    $("#h-waiting").textContent = names.length ? names.join(", ") : "-";
    // Only worth a banner if we're actually stuck.
    const stuck = names.length && this.seated && this.next - this.ackedTic >= MAX_AHEAD - 2;
    $("#banner").hidden = !stuck;
    if (stuck) $("#banner").textContent = `Waiting for ${names.join(", ")}...`;
  }
}

$("#view").addEventListener("change", ev => {
  if (!session) return;
  session.view = +ev.target.value;
  session.soundTic = -1;
});

const lockPointer = () => {
  sound.unlock();
  try { canvas.requestPointerLock?.()?.catch?.(() => {}); } catch {}
};
canvas.addEventListener("click", lockPointer);
$("#overlay").addEventListener("click", () => { session?.setPlaying(true); canvas.focus(); lockPointer(); });
document.addEventListener("pointerlockchange", () => {
  if (!document.pointerLockElement && session?.playing) session.setPlaying(false);
});
document.querySelectorAll("input[name=mode]").forEach(r => r.addEventListener("change", () => {
  // Trying another mode is trying again: if the game is still busy or
  // full, the server will say so.
  notice("");
  session?.setMode(r.value);
}));
$("#leave").addEventListener("click", () => { location.hash = ""; });
$("#quit").addEventListener("click", async () => {
  if (!confirm("End this game? The workflow will complete.")) return;
  try {
    await api("POST", gamePath(session.id, "quit"));
  } catch (e) {
    if (e.code === "busy") return alert("Someone else is playing this game, so you can't end it.");
    alert(e.message);
  }
  location.hash = "";
});

// ---------------------------------------------------------------- routing

async function route() {
  session?.stop();
  session = null;
  const m = location.hash.match(/^#\/game\/([^?]+)(\?watch)?/);
  $("#lobby").hidden = !!m;
  $("#play").hidden = !m;
  document.body.classList.toggle("playing", !!m);
  if (!m) { refreshLobby(); return; }
  $("#log").textContent = "";
  notice("");
  const id = decodeURIComponent(m[1]);
  let mode = m[2] ? "watch" : "lockstep";
  try {
    // Multiplayer games get a NetSession.
    const st = await api("GET", gamePath(id, "state"));
    if (mode !== "watch" && (st.busy || st.full)) {
      notice(st.busy ? SOMEONE_PLAYING : GAME_FULL);
      mode = "watch";
    }
    session = st.config.mode ? new NetSession(id, mode, st) : new Session(id, mode);
    await session.start();
  } catch (e) {
    (session || new Session(id, mode)).log(e.message);
  }
}

config = await api("GET", "/api/config");
$("#temporal-ui-link").href = temporalUI("/workflows");
preload().then(([, wad]) => sound.setWAD(wad));
addEventListener("hashchange", route);
setInterval(() => { if (!session) refreshLobby(); }, 3000);
route();

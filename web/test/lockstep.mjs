// Checks that the browser engine reproduces a game run by the worker.
//
//   temporal workflow query -w <id> --type sync -o json > /tmp/sync.json
//   temporal workflow query -w <id> --type frame --input 0 -o json > /tmp/frame.json  # optional
//   node web/test/lockstep.mjs /tmp/sync.json [/tmp/frame.json]
//
// This is the lockstep client's resync path: boot a fresh engine from the
// run's snapshot, feed it every ticcmd, and compare checksums with the
// worker's engine (wazero) - a different WebAssembly runtime entirely.
import { readFileSync } from "node:fs";
import { Engine, useSources } from "../engine.js";

const raw = JSON.parse(readFileSync(process.argv[2], "utf8"));
const sync = raw.queryResult ? raw.queryResult[0] : raw;
useSources(await WebAssembly.compile(readFileSync("build/doom.wasm")), new Uint8Array(readFileSync("wad/doom1.wad")));

const snapshot = sync.snapshot ? Uint8Array.from(Buffer.from(sync.snapshot, "base64")) : null;
const t0 = performance.now();
const e = await Engine.create({ ...sync.config, snapshot });
const sounds = [];
// Multiplayer games log merged tics (everyone's input, plus joins and leaves).
const tics = sync.net || sync.tics || [];
for (const t of tics) { sync.net ? e.netTics([t]) : e.tics([t]); sounds.push(...e.sounds()); }
const ms = performance.now() - t0;
const got = e.checksum(), want = sync.checksum >>> 0;
const s = e.stats();
console.log(`segment ${sync.segment}: replayed ${tics.length} tics from tic ${sync.startTic} in ${ms.toFixed(0)}ms -> tic ${s.tic}`);
console.log(`worker (wazero) checksum ${want.toString(16)}, node (V8) checksum ${got.toString(16)}`);
if (got !== want || s.tic !== sync.tic) { console.log("DESYNC"); process.exit(1); }
console.log("in sync");

// Sounds are derived from the simulation too, so the browser must hear what
// the worker heard. The frame Query returns the worker's last few seconds.
if (process.argv[3]) {
  const fr = JSON.parse(readFileSync(process.argv[3], "utf8"));
  const theirs = (fr.queryResult ? fr.queryResult[0] : fr).sounds || [];
  const since = theirs.length ? theirs[0].tic : Infinity;
  const ours = sounds.filter(ev => ev.tic >= since);
  const key = ev => JSON.stringify([ev.tic, ev.kind, ev.ch, ev.name, ev.vol, ev.sep]);
  const same = ours.length === theirs.length && ours.every((ev, i) => key(ev) === key(theirs[i]));
  console.log(`sounds: worker reported ${theirs.length} events since tic ${since}, node produced ${ours.length} (${sounds.length} in total) -> ${same ? "identical" : "DIFFERENT"}`);
  if (!same) process.exit(1);
}

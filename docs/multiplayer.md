# Multiplayer

Up to four players can play a game together, in co-op or deathmatch, and
come and go at any time. This document explains how DOOM's original
multiplayer worked, and how it maps onto the system described in
[system-architecture.md](system-architecture.md). In short: **the web
server merges everyone's input in real time, and sends it to the Workflow
about once a second.**

## How DOOM's multiplayer works: lockstep

DOOM's 1993 netcode didn't send game state over the network at all. Every
machine ran the full simulation, and they exchanged only ticcmds (see
[overview.md](overview.md)). A tic could only run once *every* player's
ticcmd for it had arrived. Since every machine ran the same deterministic
engine on the same inputs, every machine computed the same game. That's
**lockstep** netcode. `ticcmd_t` even has a `consistancy` field (sic) so
machines could notice if they diverged.

Single-player DOOM on Temporal is already lockstep with one player: the
browser and the Worker run the same engine on the same ticcmds, and
compare checksums. Multiplayer only adds one thing: something has to be
the **arbiter** that merges everyone's ticcmds, one tic at a time, into a
single log that every engine then runs.

## Who should be the arbiter?

### v1: the Workflow (too slow)

The first version made the Workflow the arbiter, which is the textbook
[actor](https://docs.temporal.io/guides/durable-gaming-sessions) design:
one Workflow owns the game session, and Updates are the only way to change
it. Each browser sent its ticcmds in a `tics` Update. The Workflow kept a
queue per player, ran tic *N* once every active player's ticcmd for tic *N*
had arrived, and each Update returned the merged tics the caller hadn't
seen yet.

It worked, and every player saw the same game, but it was noticeably
laggy. In lockstep, nobody predicts anything, so you only see your own
input once it has gone to the arbiter and come back. With the Workflow as
the arbiter, every tic of input made the trip browser → web server →
Temporal → Worker and back. An Update's round trip was about 175ms, of
which about 150ms was spent between the web server, Temporal and the
Worker (several network hops, plus a durable history write before the
Update completes). With one request in flight at a time, waiting for the
slowest player, and smoothing playback, input lag came to about 2.5 round
trips: **around 430ms** from pressing fire to seeing your shotgun go off.

Tuning couldn't fix it, because nearly every part of that delay scaled with
the round trip. Updates are built for durable, exactly-once
request/response. They aren't meant to be a 35 Hz real-time bus.

### v2: the web server (what runs now)

The fix is to take Temporal off the real-time path. The web server is
already the closest point every player reaches, about 25ms away, so it
became the arbiter:

```txt
 browser A ──ticcmds──┐                                   ┌──────────────────┐
                      ▼                                   │ DoomGame Workflow│
 browser B ──────► web server ─── "record" Update ──────► │  referee: checks │
                  (room: merges   (~once a second,        │  and re-runs the │
 browser C ──────►  ticcmds into   one in flight)         │  tics, returns a │
                    tics)                                 │  checksum        │
     ▲                │                                   └──────────────────┘
     └──merged tics───┘ (to every browser, as soon as each tic is complete)
```

- Each browser streams its ticcmds to the server over a **WebSocket**.
- The server merges them and sends each merged tic back to every browser
  **the moment it's complete**. It doesn't run DOOM; it only merges 8-byte
  structs.
- About **once a second**, the server sends everything it has played to the
  Workflow as a single `record` Update. Nothing waits for it.

The Workflow is no longer the arbiter, but it's still the **source of
truth**. It referees every tic it records, runs them through its own
engine, and returns a checksum that every browser checks against its own.
If the web server crashes, the game picks up from the last record.

### Why this is more efficient

| | v1: Workflow as arbiter | v2: web server as arbiter |
| --- | --- | --- |
| Round trip to the arbiter | ~175ms | ~25ms |
| Keypress to screen (~2.5 round trips) | ~430ms | ~60–80ms |
| Updates | ~6 per second, *per player* | ~1 per second, *per game* |
| Temporal Actions per hour, 4 players | ~144,000 | ~3,600 |
| Workflow tasks | a Workflow runs one task at a time, so players' Updates queue behind each other | one per record |

The Workflow still sees every tic. It just sees them in one-second
batches, recorded once for the whole game instead of once per player, and
off the path that players are waiting on.

## The web server: rooms and the sequencer

[`internal/netplay`](../internal/netplay) holds a **room** for each
multiplayer game someone is connected to. Browsers connect to
`GET /api/games/{id}/net?player=<id>`.

The room's **sequencer** ([`sequencer.go`](../internal/netplay/sequencer.go))
keeps four slots, one per DOOM player. Each slot has a player ID, a name,
whether they're active or away, and a queue of ticcmds that haven't run
yet. The sequencer runs tic *T* once every **active** player has a ticcmd
for it:

- Away players get an empty ticcmd, so their marine stands still while
  everyone else plays on.
- If nobody is active, no tics run, so a game where everyone has paused
  simply stops.
- Joins and leaves are part of the merged tic (`NetTic{c, j, l}`: a ticcmd
  per slot, plus join and leave bitmasks), so every engine spawns or
  removes a marine at exactly the same tic.

The server checks each player's ticcmds as they arrive with the same
`CheckCmds` the Workflow's validator uses (movement limits, no pause or
save buttons).

### The protocol

JSON messages over the WebSocket, each with a type `t` (see
[`client.go`](../internal/netplay/client.go)):

| Client → server | Effect |
| --- | --- |
| `join {name}` | Claim your slot (or a free one) and become active. The reply (`seat`) says which tic your next ticcmd runs at. |
| `cmds {cmds}` | Queue your next ticcmds. |
| `pause` | Become away: the room plays empty ticcmds for you. |
| `leave` | Free your slot; your marine is removed. (If you're the last one in, you become away instead, since DOOM can't run with nobody in the game.) |
| `resync` | Send me a `sync` message again. |
| `ping` | Answered with `pong`. |

| Server → client | Meaning |
| --- | --- |
| `sync` | The Workflow's `sync` Query, plus `pending`: the tics the room has played but not recorded yet. |
| `tics {from, tics}` | Merged tics, in order and without gaps. |
| `seat` | Your slot, whether you're playing, and the tic your next ticcmd runs at. |
| `check` | The Workflow's checksum at a tic, once it has recorded it. |
| `roster` | The players, and who the game is waiting for. |
| `error`, `over` | Something went wrong (`code: "full"` if there was no slot), or the game ended. |

### Liveness

The server has a real clock, so liveness is a few simple timers instead of
anything in the Workflow:

- Closing the socket (closing the tab, losing the network) makes you away.
- An active player who holds up everyone else for **3 seconds** without
  sending input is made away, so one player can't freeze the game.
- Away players are removed after **5 minutes**.

## The browser: lockstep, the 1993 way

The browser runs the engine but only advances it on merged tics from the
server, rendering from its own player's view. There's no prediction, so
input lag is about one round trip to the server plus waiting for the
slowest player's input. The client stops making ticcmds once it's more than
20 tics ahead of the game, and shows who it's waiting for.

Every `check` from the Workflow is compared with the browser's own checksum
for that tic. A mismatch, or a `check` from a new run of the Workflow
(after Continue-as-New), makes the browser resync.

Escape pauses you (`pause`), and the engine keeps running the live game
behind the overlay. Clicking to resume sends `join`. The player ID lives in
`sessionStorage`, so a refresh keeps your marine, and a second tab is a
second player.

## The Workflow: recorder and referee

A multiplayer `DoomGame` registers a `record` Update instead of `tics`
([`internal/game/multiplayer.go`](../internal/game/multiplayer.go)):

| | |
| --- | --- |
| **Input** | `NetRecord{startTic, tics, slots}`: the merged tics since the last record, and the roster as of the last one. |
| **Validator** | `startTic` must equal the game's tic (idempotency). Every ticcmd must be legal, and joins, leaves and input may only involve players who are (or are becoming) in the game. The roster must agree with the tics. The Workflow doesn't take the server's word for any of it. |
| **Handler** | Runs the tics through the Workflow's engine, counts deaths, posts finished levels to the leaderboard, and returns the stats and checksum. |

It also answers a `net` Query (the roster with player IDs, for the server),
and its `frame` Query takes a `view` argument so spectators and thin
clients can watch any player, about a second behind the live game. Records
are batched, so it continues as new after 300 of them (about 5 minutes),
carrying the roster into the next run.

### Recording and recovery

- **One record in flight.** The room sends a record about once a second
  (`FlushEvery`), and never has more than one in flight. When it returns,
  the room passes the Workflow's checksum to the browsers as a `check`.
- **Failed records.** If a record fails, the room asks the Workflow where
  it is (`net` Query). If the record went through after all, it carries on.
  If the Workflow's game isn't the room's (it was Reset, say), the room
  starts over from the Workflow, and every browser resyncs.
- **Server restarts.** A new room loads the roster and tic from the
  Workflow, with everyone away. Browsers reconnect, resync and rejoin. The
  game loses whatever wasn't recorded yet, about a second.

### Syncing a browser

A browser's engine needs the run's snapshot, every tic since, and the tics
the room has played but not recorded yet. The room runs the `sync` Query,
appends the unrecorded tics (`pending`), and only then starts sending the
browser live tics, so there's never a gap.

## Changes to the engine

Vanilla DOOM expected every player to be present when a level started.
gore's step API added what multiplayer needs:

- `StepNetTic` runs one tic for every player in the game. Membership changes
  are part of the tic: a joining player spawns at their start (or another
  free one) with a teleport fog, and a leaving player's marine is removed.
- `StepSetView` picks whose eyes and status bar to render. This only
  affects drawing and sound, never the simulation, so players watching
  different views can't desync.
- `Checksum` covers every player in the game.
- Saving is allowed while a player is dead, so one dead, AFK player can't
  block Continue-as-New forever.
- `-solo-net` turns on netgame rules (co-op respawns, multiplayer-only
  things) without any networking, and `-deathmatch` turns on deathmatch.

## Limits

- **One web server.** A game's room lives in one process. Running more
  than one server would mean routing every player of a game to the same
  one.
- **The slowest player sets the pace.** A tic runs only once every
  player's input for it has arrived. The game doesn't get *slower* (it
  still runs 35 tics a second), but it gets *later*: your lag is roughly
  the slowest player's one-way time to the server plus your own one-way
  time back. A player whose connection stalls freezes everyone until they
  catch up, or until the 3-second away timer kicks in.
- **No prediction.** Two standard techniques would fix the rest. A
  deadline for late input on the server (fill in a late player's previous
  ticcmd and move on) would stop nearby players waiting for distant or
  jittery ones. Client-side prediction with rollback (run ahead with a guess
  for other players' input, and re-simulate when the real tic arrives) would
  make your own input feel instant. DOOM suits both: it's deterministic,
  tics are cheap to simulate, and the engine can already snapshot itself.
- **No chat.** `ticcmd_t.chatchar` exists, but the validators would need
  to allow it.

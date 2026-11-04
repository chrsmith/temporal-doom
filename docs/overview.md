# What does "DOOM on Temporal" mean?

A game of DOOM is a Temporal Workflow. The DOOM engine runs *inside* the
Workflow code, the player's input is recorded in the Workflow's Event
History, and Temporal can rebuild the game at any point by replaying that
input. Kill the Worker mid-fight and start another one: the game carries on
exactly where it was, and the player never had to save.

This works because DOOM and Temporal are built on the same idea: **if the
logic is deterministic, the inputs are enough to recover the state.** This
document explains that idea. For how the pieces fit together, see
[system-architecture.md](system-architecture.md). For multiplayer, see
[multiplayer.md](multiplayer.md).

## How Temporal makes code durable

A [Workflow](https://docs.temporal.io/workflows) is ordinary code. As it
runs, Temporal records every input it receives and every result it gets
back in an [Event History](https://docs.temporal.io/encyclopedia/event-history).
Temporal never saves the Workflow's memory. If the
[Worker](https://docs.temporal.io/workers) running it dies, another Worker
re-runs the same code against the same history and ends up in the same
state.

That only works if Workflow code is
[deterministic](https://docs.temporal.io/workflow-definition#deterministic-constraints):
given the same history, it must make the same decisions every time. No
reading the clock, no random numbers, no I/O, unless it goes through
Temporal so the result is recorded.

So to run something on Temporal, you need to break it into **discrete steps
whose inputs Temporal can record**, and make sure that replaying those
inputs reproduces the state.

## Why not just save the game's memory?

DOOM needs about 4 MiB of RAM, so the obvious approach is to snapshot the
engine's memory as the Workflow's state. But DOOM updates its state 35
times a second. That's around 140 MiB of snapshots per second, or over
8 GiB a minute. It doesn't scale.

## DOOM is already deterministic

id Software solved this in 1993. DOOM's simulation is entirely
deterministic: it uses fixed-point math instead of floating point, and its
"random" numbers come from a fixed 256-entry table. The game advances in
fixed steps called **tics**, 35 per second, and each tic takes exactly one
small input per player.

That input is [`ticcmd_t`](https://github.com/id-Software/DOOM/blob/master/linuxdoom-1.10/d_ticcmd.h#L32-L44):

```c
typedef struct
{
    char  forwardmove;  // *2048 for move
    char  sidemove;     // *2048 for move
    short angleturn;    // <<16 for angle delta
    short consistancy;  // checks for net game
    byte  chatchar;
    byte  buttons;
} ticcmd_t;
```

Give the engine the same starting map and the same sequence of ticcmds, and
it reaches the same state, bit for bit. This is how DOOM demos (`.lmp`
files) work, including the one that plays on the title screen: a demo is a
starting map plus a list of ticcmds, and the engine replays the whole game
by reading them one at a time.

In this project, a ticcmd is 8 bytes on the wire: forward and side
movement, turn, and buttons (fire, use, change weapon). See `TicCmd` in
[`internal/doomvm/doomvm.go`](../internal/doomvm/doomvm.go).

## A DOOM demo is a Workflow history

DOOM was event-sourced decades before the term caught on. The concepts map
almost one to one:

| DOOM | Temporal |
| --- | --- |
| a tic | a step of the Workflow |
| a `ticcmd` | an input recorded in history |
| demo file (`.lmp`) | Workflow history |
| demo playback (`doom -playdemo`) | Workflow replay |
| demo desync | non-determinism error |
| savegame | Continue-as-New input |
| "this demo needs v1.9" | pinning a Workflow to a version |

So "porting DOOM to Temporal" means:

1. **The engine runs inside the Workflow.** The game state lives in
   Workflow memory, not in a database.
2. **Ticcmds are the only input.** They arrive as
   [Workflow Updates](https://docs.temporal.io/encyclopedia/workflow-message-passing),
   each carrying a batch of ticcmds. The Update handler runs them through
   the engine, and Temporal records them in history.
3. **Replay rebuilds the game.** A Worker that has never seen the game
   boots a fresh engine and runs the recorded ticcmds through it, exactly
   like playing back a demo.

The Workflow has no game loop and no timers. The game only advances when
ticcmds arrive, so the client is the clock. Pausing is just the client not
sending anything, and an idle game costs nothing.

## Making the engine safe to run in a Workflow

The engine is [gore](https://github.com/AndreRenaud/gore), a pure Go port
of DOOM. DOOM's *simulation* is deterministic, but its *main loop* isn't: it
reads the wall clock to decide how many tics to run. gore has a **step
mode** for this project that never reads the clock. The host calls
`StepTic` with a ticcmd and the engine runs exactly one tic.

gore also keeps all of its state in global variables (a leftover from the
C it was translated from), so it can only run one game per process. To run
many games in one Worker, the engine is compiled to WebAssembly
(`build/doom.wasm`), and each game runs in its own instance under
[wazero](https://wazero.io/), a pure Go WebAssembly runtime. The sandbox
has no real clock, no filesystem and no network, so the engine *can't* be
non-deterministic, even by accident. The only way to affect the game is to
feed it ticcmds.

The same `doom.wasm` also runs in the browser. Two unrelated WebAssembly
runtimes (wazero in the Worker, V8 in the browser) produce the same game,
bit for bit, which the checksums prove.

## Determinism is the whole game

Everything above depends on one property: the same ticcmds always produce
the same game. That's why:

- **Everything is checksummed.** The engine computes a checksum of the
  game state, and every Update returns the checksum after its batch.
  Clients compare it with their own engine's checksum at the same tic, and
  resync if they ever disagree.
- **The engine is pinned per game.** Changing the engine (or the WAD)
  changes how it responds to the same ticcmds, which is exactly a demo
  desync. Each game records a hash of the engine it started on (with
  `workflow.SideEffect`), and a Worker running a different engine refuses
  to replay it. The Workflow task fails and Temporal retries it until a
  Worker with the right engine picks it up. See
  [`make purge-all-games`](../Makefile) for the blunt way to upgrade, and
  [Worker Versioning](https://docs.temporal.io/production-deployment/worker-deployments/worker-versioning)
  for the proper one.
- **Workflow logic follows the same rules.** Changing how the Workflow
  reacts to recorded input breaks replay of games in flight. Try it:
  change gameplay code in `internal/game/workflow.go`, then run
  `./build/replay` on an old game, and it reports the tic where the game
  diverges.

## What you get for free

Once the game is a Workflow, Temporal's features apply to it directly:

- **Crash recovery.** Kill the Worker (`kill -9`) and start it again. The
  new Worker replays the history and the game continues. The player sees
  one slow Update.
- **Unbounded games.** A Workflow run's history can't grow forever, so
  every so often (after 1,000 Updates, or sooner if Temporal suggests it)
  the Workflow saves the game with DOOM's own savegame
  code (about 25 KB) and
  [continues as new](https://docs.temporal.io/workflow-execution/continue-as-new)
  with the savegame as input. A savegame is exactly "the whole game in a
  small input".
- **Time travel.** Temporal's
  [Reset](https://docs.temporal.io/cli/workflow#reset) rewinds a Workflow
  to an earlier event. In DOOM, that undoes your death:

  ```sh
  temporal workflow reset -w <id> -e <eventId> --reapply-exclude Update --reason rewind
  ```

  `--reapply-exclude Update` matters: without it, Temporal re-applies the
  later Updates and you get the same game (and the same fireball) back.
- **Visibility.** Each game keeps search attributes up to date, so you
  can search for games with `DoomMap = 'E1M1' AND DoomHealth < 50`, and
  the Temporal UI shows every ticcmd batch in the game's history.

None of this needed special support from Temporal. The DOOM Workflow is
built from the same pieces as any other Workflow, so the guarantees that
let a 1993 shooter survive a `kill -9` are the same ones that keep a
payment pipeline running when a machine dies.

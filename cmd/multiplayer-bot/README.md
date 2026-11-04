# multiplayer-bot

A dumb bot for simulating multiplayer clients.

It isn't trying to play DOOM. It starts or joins a multiplayer game, then runs
around shooting at random, so you can test multiplayer without rounding up
three friends. That's handy because the browser client grabs the mouse,
which makes it hard to drive several browser players by yourself.

Each bot:

- Picks a new direction every 2-4 seconds, turning to a new heading and
  then running, strafing or backing up.
- Fires for a moment every 2-5 seconds, and sometimes presses *use* as it
  sets off, which opens any door it's facing.
- Respawns about a second after it dies.
- Leaves the game when you press Ctrl-C (or when `--duration` is up), which
  frees its slot.

## Running it

It's a single Python script with one dependency (`websockets`), declared
inline, so [uv](https://docs.astral.sh/uv/) installs it for you. With the
server running (`make server-dev`, `make worker` and `make web`):

```sh
uv run cmd/multiplayer-bot/multiplayer_bot.py                    # join the newest multiplayer game, or start a co-op game if there isn't one
uv run cmd/multiplayer-bot/multiplayer_bot.py doom-1234abcd      # join a specific game
uv run cmd/multiplayer-bot/multiplayer_bot.py --new deathmatch   # start a new game (--skill 1-5 sets its difficulty)
uv run cmd/multiplayer-bot/multiplayer_bot.py --bots 3           # add three players at once
uv run cmd/multiplayer-bot/multiplayer_bot.py --server https://doom.example.com   # a deployed server
```

Then open the game in your browser and join it as the fourth player. DOOM
allows four players in a game, so a bot that finds the game full gives up.

Other flags: `--name` sets the bots' name prefix (default `pybot`), and
`--duration` plays for that many seconds and then leaves.

## How it works

The bot speaks the same WebSocket protocol as the browser (see
[`internal/netplay/client.go`](../../internal/netplay/client.go)). It
connects to the web server's room for the game, claims a slot with `join`,
and streams its ticcmds with `cmds` at 35 tics a second. Like the browser, it
never gets more than 20 tics ahead of the merged tics the server sends back,
so a slow player holds it up just as it would hold up a person.

Unlike the browser, it doesn't run DOOM. It never sees the game, so it can't
aim, and it can't check the workflow's checksums. It learns that it died by
polling the server's `state` endpoint, which is the workflow's view of the
game and about a second behind it.

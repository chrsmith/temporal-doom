#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = ["websockets>=13"]
# ///
"""A dumb bot for simulating multiplayer clients.

It doesn't try to play DOOM well, or at all really. It starts or joins a
multiplayer game and runs around shooting randomly, so you can test
multiplayer with more players than you have hands. See README.md.

Each bot connects to the web server's room for a game over the same
WebSocket protocol as the browser (see internal/netplay/client.go), claims a
slot, and streams random input at 35 tics a second: it changes direction
every few seconds, fires every few seconds, and respawns about a second
after it dies.

Unlike the browser, it doesn't run DOOM. It only sends ticcmds
and counts the merged tics the server sends back, to pace itself. So it
can't check checksums, and it learns that it died from the server's state
endpoint (the workflow's view, about a second behind the live game).

Usage:

    uv run cmd/multiplayer-bot/multiplayer_bot.py                    # join the newest multiplayer game (or start one)
    uv run cmd/multiplayer-bot/multiplayer_bot.py doom-1234abcd      # join a specific game
    uv run cmd/multiplayer-bot/multiplayer_bot.py --new deathmatch   # start a new game
    uv run cmd/multiplayer-bot/multiplayer_bot.py --bots 3           # three players at once

Ctrl-C leaves the game, freeing the slots.
"""

import argparse
import asyncio
import json
import random
import secrets
import time
import urllib.parse
import urllib.request

from websockets.asyncio.client import connect
from websockets.exceptions import ConnectionClosed

TIC_RATE = 35  # tics per second
MAX_AHEAD = 20  # don't get further than this ahead of the game (as in app.js)
MAX_MOVE = 0x32  # the largest legal forward/side movement (game.MaxMove)
TURN_FAST = 1280  # DOOM's fast keyboard turn, per tic

BUTTON_ATTACK = 1
BUTTON_USE = 2


def get_json(server, path):
    with urllib.request.urlopen(server + path, timeout=10) as resp:
        return json.load(resp)


def start_game(server, mode, skill, player):
    """Starts a new multiplayer game, and returns its ID."""
    body = json.dumps({"player": player, "mode": mode, "skill": skill}).encode()
    req = urllib.request.Request(server + "/api/games", data=body, method="POST", headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)["id"]


def pick_game(server):
    """Returns the ID of the newest running multiplayer game, or None."""
    games = [g for g in get_json(server, "/api/games")["games"] if g.get("mode") in ("coop", "deathmatch")]
    if not games:
        return None
    games.sort(key=lambda g: g.get("started", ""), reverse=True)
    return games[0]["id"]


class Brain:
    """Decides what a bot does each tic: random wandering and shooting."""

    def __init__(self, rng):
        self.rng = rng
        self.now = 0.0
        self.next_move = 0.0  # when to pick a new direction
        self.forward = self.side = 0
        self.turn_tics = self.turn = 0
        self.next_fire = rng.uniform(1, 3)
        self.fire_tics = 0
        self.use_tics = 0

    def new_direction(self):
        rng = self.rng
        self.forward = rng.choice([MAX_MOVE, MAX_MOVE, 25, 0, -25])
        self.side = rng.choice([0, 0, 24, -24, 40, -40])
        # Turn to a new heading over the next few tics.
        self.turn = rng.choice([TURN_FAST, -TURN_FAST])
        self.turn_tics = rng.randint(3, 25)
        # Tap use as we set off, which opens any door we're facing.
        self.use_tics = 1 if rng.random() < 0.3 else 0
        self.next_move = self.now + rng.uniform(2, 4)

    def cmd(self, now):
        """Returns the next ticcmd, in the server's JSON form."""
        self.now = now
        if now >= self.next_move:
            self.new_direction()
        if now >= self.next_fire:
            self.fire_tics = self.rng.randint(5, 20)
            self.next_fire = now + self.rng.uniform(2, 5)

        c = {"f": self.forward, "s": self.side, "t": 0, "b": 0}
        if self.turn_tics > 0:
            self.turn_tics -= 1
            c["t"] = self.turn
        if self.fire_tics > 0:
            self.fire_tics -= 1
            c["b"] |= BUTTON_ATTACK
        if self.use_tics > 0:
            self.use_tics -= 1
            c["b"] |= BUTTON_USE
        # Leave out zeros, as the server does.
        return {k: v for k, v in c.items() if v}

    def respawn(self):
        """Presses use for a few tics, which respawns a dead marine."""
        self.use_tics = 5


class Bot:
    def __init__(self, server, game, name, player_id, seed):
        self.server = server
        self.game = game
        self.name = name
        self.player_id = player_id
        self.brain = Brain(random.Random(seed))
        self.slot = -1
        self.playing = False
        self.have = None  # the game's tic, as far as we've seen
        self.next = 0  # the tic our next ticcmd runs at
        self.joined = False
        self.stop = False
        self.ws = None
        self.respawn_at = None
        self.respawn_cooldown = 0.0
        self.sent = self.deaths = 0

    def log(self, msg):
        print(f"{time.strftime('%H:%M:%S')} {self.name}: {msg}", flush=True)

    async def send(self, msg):
        await self.ws.send(json.dumps(msg))

    async def run(self):
        """Plays until stopped, reconnecting if the connection drops."""
        while not self.stop:
            try:
                await self.play()
            except (ConnectionClosed, OSError) as e:
                if self.stop:
                    break
                self.log(f"disconnected ({e}); reconnecting in 2s")
                await asyncio.sleep(2)

    async def play(self):
        base = urllib.parse.urlsplit(self.server)
        scheme = "wss" if base.scheme == "https" else "ws"
        path = f"/api/games/{urllib.parse.quote(self.game)}/net"
        query = urllib.parse.urlencode({"player": self.player_id})
        url = urllib.parse.urlunsplit((scheme, base.netloc, path, query, ""))
        # Sync messages carry a savegame, so allow big messages.
        async with connect(url, max_size=None, compression="deflate") as ws:
            self.ws, self.joined, self.have, self.playing = ws, False, None, False
            tasks = [asyncio.create_task(t) for t in (self.receive(), self.input_loop(), self.watch_health())]
            try:
                done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
                for t in done:
                    t.result()  # raise the error, if any
            finally:
                for t in tasks:
                    t.cancel()
                if self.stop:
                    try:
                        await self.send({"t": "leave"})
                    except ConnectionClosed:
                        pass

    async def receive(self):
        async for raw in self.ws:
            m = json.loads(raw)
            t = m.get("t")
            if t == "sync":
                self.have = m["tic"] + len(m.get("pending") or [])
                if not self.joined:
                    self.joined = True
                    await self.send({"t": "join", "name": self.name})
            elif t == "tics":
                end = m["from"] + len(m["tics"])
                if self.have is None or end > self.have:
                    self.have = end
            elif t == "seat":
                if m["playing"] and not self.playing:
                    self.log(f"playing as player {m['slot'] + 1} from tic {m['tic']}")
                self.slot, self.playing, self.next = m["slot"], m["playing"], m["tic"]
            elif t == "error":
                self.log(f"server error: {m.get('error')}")
                if m.get("code") == "full":
                    self.stop = True
                    return
            elif t == "over":
                self.log(f"game over: {m.get('reason')}")
                self.stop = True
                return

    async def input_loop(self):
        """Sends input at 35 tics a second, but no more than MAX_AHEAD tics
        past the game, so a slow game (or a slow player) holds us back."""
        clock, made = time.monotonic(), 0
        while True:
            await asyncio.sleep(1 / TIC_RATE)
            now = time.monotonic()
            if not self.playing or self.have is None:
                clock, made = now, 0
                continue
            if self.respawn_at is not None and now >= self.respawn_at:
                self.respawn_at = None
                self.respawn_cooldown = now + 3  # the state lags; don't press twice
                self.brain.respawn()
                self.log("pressing use to respawn")
            cmds = []
            due = int((now - clock) * TIC_RATE) - made
            for _ in range(due):
                made += 1
                if self.next - self.have < MAX_AHEAD:
                    cmds.append(self.brain.cmd(now))
                    self.next += 1
            if cmds:
                await self.send({"t": "cmds", "cmds": cmds})
                self.sent += len(cmds)

    async def watch_health(self):
        """Polls the game's state for our health, to notice that we died."""
        path = f"/api/games/{urllib.parse.quote(self.game)}/state"
        dead = False
        while True:
            await asyncio.sleep(1)
            if self.slot < 0:
                continue
            try:
                st = await asyncio.to_thread(get_json, self.server, path)
            except OSError as e:
                self.log(f"state: {e}")
                continue
            me = next((p for p in st.get("players") or [] if p["slot"] == self.slot), None)
            if me is None:
                continue
            now_dead = me["health"] <= 0
            if now_dead and not dead:
                self.deaths += 1
                self.log(f"died (death #{self.deaths}, kills {me.get('kills', 0)})")
            elif dead and not now_dead:
                self.log(f"respawned (health {me['health']})")
            dead = now_dead
            if dead and self.respawn_at is None and time.monotonic() >= self.respawn_cooldown:
                self.respawn_at = time.monotonic() + 1


async def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("game", nargs="?", help="workflow ID of the game (default: the newest multiplayer game, or a new co-op game if there isn't one)")
    p.add_argument("--new", choices=["coop", "deathmatch"], help="start a new game in this mode, instead of joining one")
    p.add_argument("--skill", type=int, default=3, help="skill for a new game, 1 (I'm too young to die) to 5 (Nightmare!)")
    p.add_argument("--server", default="http://localhost:8080", help="web server URL")
    p.add_argument("--bots", type=int, default=1, help="how many players to add (DOOM allows 4 in a game)")
    p.add_argument("--name", default="pybot", help="player name prefix")
    p.add_argument("--duration", type=float, default=0, help="seconds to play, then leave (default: until Ctrl-C)")
    args = p.parse_args()

    server = args.server.rstrip("/")
    if args.game and args.new:
        p.error("give a game to join, or --new, not both")
    game = args.game or (None if args.new else pick_game(server))
    if game is None:
        mode = args.new or "coop"
        game = start_game(server, mode, args.skill, args.name)
        print(f"started a new {mode} game", flush=True)
    print(f"joining {game} on {server} with {args.bots} bot(s); Ctrl-C to leave", flush=True)

    bots = []
    for i in range(args.bots):
        suffix = secrets.token_hex(2)
        name = f"{args.name}-{suffix}" if args.bots == 1 else f"{args.name}{i + 1}-{suffix}"
        bots.append(Bot(server, game, name, "py-" + secrets.token_hex(8), seed=secrets.randbits(32)))

    async def start(i, bot):
        await asyncio.sleep(i * 0.7)  # arrive one by one
        await bot.run()

    tasks = [asyncio.create_task(start(i, b)) for i, b in enumerate(bots)]
    try:
        if args.duration > 0:
            await asyncio.wait(tasks, timeout=args.duration)
        else:
            await asyncio.gather(*tasks)
    finally:
        # Leave politely, so our marines don't linger as "away".
        for b in bots:
            b.stop = True
            if b.ws is not None:
                try:
                    await b.send({"t": "leave"})
                    await b.ws.close()
                except ConnectionClosed:
                    pass
        for t in tasks:
            t.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        for b in bots:
            print(f"{b.name}: sent {b.sent} ticcmds, died {b.deaths} times", flush=True)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass

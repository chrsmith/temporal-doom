package main

import (
	"context"
	"sync"
	"time"
)

// playerHeader carries the browser's player ID: random, and per tab, like
// the one a multiplayer client connects with.
const playerHeader = "X-Doom-Player"

// leaseTTL is how long a single-player game stays someone's after their
// last input. Once they've gone quiet for this long (paused, or left), the
// next player to join takes over.
const leaseTTL = 10 * time.Second

// leases remember who is playing each single-player game, so that someone
// who joins from the lobby can't play over them. This guards against
// accidents, not attackers: player IDs aren't secret credentials, just
// random IDs that never leave the browser that made them.
//
// Leases live in memory, which is fine because there's only one server
// (see netplay). A restart forgets them, and anyone can take over for up
// to leaseTTL.
type leases struct {
	mu sync.Mutex
	m  map[string]lease
}

type lease struct {
	player string
	seen   time.Time
}

func newLeases() *leases { return &leases{m: map[string]lease{}} }

// claim makes a game player's for another leaseTTL, unless someone else
// has played it more recently than that.
func (l *leases) claim(id, player string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[id]; ok && cur.player != player && now.Sub(cur.seen) < leaseTTL {
		return false
	}
	l.m[id] = lease{player: player, seen: now}
	return true
}

// busy reports whether someone other than player is playing a game. With
// no player, it reports whether anyone is.
func (l *leases) busy(id, player string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.m[id]
	return ok && (player == "" || cur.player != player) && now.Sub(cur.seen) < leaseTTL
}

func (l *leases) release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
}

// sweep forgets expired leases every interval, until ctx is done.
func (l *leases) sweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.mu.Lock()
			for id, cur := range l.m {
				if now.Sub(cur.seen) >= leaseTTL {
					delete(l.m, id)
				}
			}
			l.mu.Unlock()
		}
	}
}

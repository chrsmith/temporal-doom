package main

import (
	"testing"
	"time"
)

func TestLeases(t *testing.T) {
	l := newLeases()
	t0 := time.Now()
	if !l.claim("g", "alice", t0) {
		t.Fatal("alice can't claim a free game")
	}
	if !l.claim("g", "alice", t0.Add(time.Second)) {
		t.Fatal("alice can't keep playing her own game")
	}
	if l.claim("g", "bob", t0.Add(5*time.Second)) {
		t.Fatal("bob took over while alice was playing")
	}
	if !l.busy("g", "bob", t0.Add(5*time.Second)) || !l.busy("g", "", t0.Add(5*time.Second)) {
		t.Error("game isn't busy while alice is playing")
	}
	if l.busy("g", "alice", t0.Add(5*time.Second)) {
		t.Error("game is busy for alice, who is the one playing it")
	}
	// Alice last played at t0+1s, so the game is free from t0+11s.
	if l.busy("g", "bob", t0.Add(11*time.Second)) {
		t.Error("game is still busy after alice went quiet")
	}
	if !l.claim("g", "bob", t0.Add(11*time.Second)) {
		t.Fatal("bob can't take over after alice went quiet")
	}
	if l.claim("g", "alice", t0.Add(12*time.Second)) {
		t.Fatal("alice took the game back from bob")
	}
	l.release("g")
	if l.busy("g", "", t0.Add(12*time.Second)) || !l.claim("g", "alice", t0.Add(12*time.Second)) {
		t.Error("released game is still busy")
	}
}

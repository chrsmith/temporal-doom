package game

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"go.temporal.io/sdk/activity"
)

// Activities are the game's side effects. Workflow code must be
// deterministic; anything that touches the outside world lives here.
type Activities struct {
	// LeaderboardPath is a JSON-lines file of completed levels.
	LeaderboardPath string

	mu sync.Mutex
}

// RecordLevel appends a completed level to the leaderboard.
func (a *Activities) RecordLevel(ctx context.Context, r LevelResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	activity.GetLogger(ctx).Info("level completed", "player", r.Player, "map", r.Map, "kills", r.Kills, "time", r.TimeTics/35)
	if err := os.MkdirAll(filepath.Dir(a.LeaderboardPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(a.LeaderboardPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(r)
}

// ReadLeaderboard returns completed levels, fastest first per map.
func ReadLeaderboard(path string) ([]LevelResult, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LevelResult
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var r LevelResult
		if err := dec.Decode(&r); err != nil {
			break
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Map != out[j].Map {
			return out[i].Map < out[j].Map
		}
		return out[i].TimeTics < out[j].TimeTics
	})
	return out, nil
}

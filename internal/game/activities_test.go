package game

import (
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

func TestLeaderboard(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	a := &Activities{LeaderboardPath: filepath.Join(t.TempDir(), "board.jsonl")}
	env.RegisterActivity(a)
	for _, r := range []LevelResult{
		{Player: "slow", Map: "E1M1", TimeTics: 35 * 90},
		{Player: "fast", Map: "E1M1", TimeTics: 35 * 30},
		{Player: "other", Map: "E1M2", TimeTics: 35 * 10},
	} {
		if _, err := env.ExecuteActivity(a.RecordLevel, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := ReadLeaderboard(a.LeaderboardPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Player != "fast" || rows[1].Player != "slow" || rows[2].Map != "E1M2" {
		t.Fatalf("unexpected leaderboard order: %+v", rows)
	}
}

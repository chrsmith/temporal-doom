package main

import "testing"

func TestSanitizeSearch(t *testing.T) {
	ok := map[string]string{
		"DoomMap = 'E1M1' AND DoomHealth < 50":                "DoomMap = 'E1M1' AND DoomHealth < 50",
		`DoomMap="E1M8" and (DoomKills>=10 or DoomDeaths!=0)`: "DoomMap = 'E1M8' AND ( DoomKills >= 10 OR DoomDeaths != 0 )",
		"DoomPlayers IN ('alice', 'bob')":                     "DoomPlayers IN ( 'alice' , 'bob' )",
		"DoomHealth BETWEEN 0 AND 20":                         "DoomHealth BETWEEN 0 AND 20",
		"DoomPlayer STARTS_WITH 'mar'":                        "DoomPlayer STARTS_WITH 'mar'",
		"DoomMode IS NOT NULL":                                "DoomMode IS NOT NULL",
		"DoomHealth > -1":                                     "DoomHealth > -1",
	}
	for in, want := range ok {
		got, err := sanitizeSearch(in)
		if err != nil {
			t.Errorf("sanitizeSearch(%q): %v", in, err)
		} else if got != want {
			t.Errorf("sanitizeSearch(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		// Escaping the server's filter.
		"1=1) OR (ExecutionStatus != 'Running'",
		"DoomHealth < 50) OR (DoomHealth > 0",
		"(DoomHealth < 50",
		// Fields other than the game's search attributes.
		"WorkflowType = 'Other'",
		"ExecutionStatus = 'Completed'",
		"WorkflowId = 'x'",
		// Breaking out of string literals.
		`DoomMap = 'E1M1\' OR 1=1`,
		`DoomMap = 'it"s'`,
		"DoomMap = 'E1M1",
		// Anything else.
		"DoomHealth < 50; DROP",
		"DoomHealth === 5",
		"DoomHealth > -",
		"DoomMap = 'a'\nOR",
		string(make([]byte, maxSearch+1)),
	}
	for _, in := range bad {
		if got, err := sanitizeSearch(in); err == nil {
			t.Errorf("sanitizeSearch(%q) = %q, want an error", in, got)
		}
	}
}

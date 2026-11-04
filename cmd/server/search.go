package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/chrsmith/temporal-doom/internal/game"
)

// maxSearch is the longest search the lobby accepts.
const maxSearch = 256

// searchAttributes are the only fields a lobby search can mention. Without
// this, a search could name WorkflowType or ExecutionStatus and list other
// workflows in the namespace.
var searchAttributes = map[string]bool{
	game.SAPlayer.GetName():  true,
	game.SAMap.GetName():     true,
	game.SAHealth.GetName():  true,
	game.SAKills.GetName():   true,
	game.SADeaths.GetName():  true,
	game.SAMode.GetName():    true,
	game.SAPlayers.GetName(): true,
}

var searchKeywords = map[string]bool{
	"AND": true, "OR": true, "NOT": true, "IN": true, "BETWEEN": true,
	"STARTS_WITH": true, "IS": true, "NULL": true,
}

// sanitizeSearch checks a lobby search, e.g. DoomMap = 'E1M8' AND
// DoomHealth < 20, and returns it rebuilt from its tokens. The server ANDs
// it onto its own filter, so it may only use the game's search attributes,
// literals, comparisons and keywords, and its parentheses must balance:
// otherwise "x) OR (y" would escape the filter.
func sanitizeSearch(q string) (string, error) {
	if len(q) > maxSearch {
		return "", fmt.Errorf("search is longer than %d characters", maxSearch)
	}
	var toks []string
	depth := 0
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t':
			i++
			continue
		case c == '(' || c == ')' || c == ',':
			switch c {
			case '(':
				depth++
			case ')':
				if depth--; depth < 0 {
					return "", errors.New("unbalanced parentheses")
				}
			}
			toks = append(toks, string(c))
			i++
		case c == '\'' || c == '"':
			end := strings.IndexByte(q[i+1:], c)
			if end < 0 {
				return "", errors.New("unterminated string")
			}
			s := q[i+1 : i+1+end]
			if strings.ContainsAny(s, "'\"\\\n\r") {
				return "", errors.New("strings can't contain quotes, backslashes or newlines")
			}
			toks = append(toks, "'"+s+"'")
			i += end + 2
		case strings.ContainsRune("=!<>", rune(c)):
			j := i + 1
			for j < len(q) && strings.ContainsRune("=<>", rune(q[j])) {
				j++
			}
			op := q[i:j]
			switch op {
			case "=", "!=", "<>", "<", "<=", ">", ">=":
			default:
				return "", fmt.Errorf("unsupported operator %q", op)
			}
			toks = append(toks, op)
			i = j
		case c == '-' || isDigit(c):
			j := i + 1
			for j < len(q) && isDigit(q[j]) {
				j++
			}
			if q[i:j] == "-" {
				return "", errors.New("expected a number after -")
			}
			toks = append(toks, q[i:j])
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(q) && (isIdentStart(q[j]) || isDigit(q[j])) {
				j++
			}
			word := q[i:j]
			switch {
			case searchKeywords[strings.ToUpper(word)]:
				toks = append(toks, strings.ToUpper(word))
			case searchAttributes[word]:
				toks = append(toks, word)
			default:
				return "", fmt.Errorf("unknown search attribute %q", word)
			}
			i = j
		default:
			return "", fmt.Errorf("unexpected character %q", c)
		}
	}
	if depth != 0 {
		return "", errors.New("unbalanced parentheses")
	}
	return strings.Join(toks, " "), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

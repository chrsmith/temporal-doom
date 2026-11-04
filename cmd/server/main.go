// Command server serves the browser client and proxies it to Temporal.
//
// Almost every endpoint maps onto one Temporal primitive:
//
//	POST /api/games              start a DoomGame workflow
//	GET  /api/games              list running games (visibility + search attributes)
//	POST /api/games/{id}/tics    the "tics" Update (single player)
//	GET  /api/games/{id}/frame   the "frame" Query (PNG, plus sounds in a header)
//	GET  /api/games/{id}/sync    the "sync" Query (snapshot + ticcmds)
//	GET  /api/games/{id}/state   the "state" Query
//	POST /api/games/{id}/quit    the "quit" Update
//
// The exception is multiplayer, which is too latency-sensitive to go
// through Temporal on every tic. The server runs those games itself, and
// records them in the workflow about once a second (see package netplay):
//
//	GET  /api/games/{id}/net     WebSocket: join and play a multiplayer game
//
// That makes the server stateful, so there can only be one of it.
//
// Browsers send their player ID (random, per tab) in an X-Doom-Player
// header. Anyone can start, watch, join or quit a game, except that nobody
// can play a single-player game while someone else is playing it, or quit
// a game that someone else is playing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
	"github.com/chrsmith/temporal-doom/internal/game"
	"github.com/chrsmith/temporal-doom/internal/netplay"
	"github.com/chrsmith/temporal-doom/internal/setup"
	"github.com/chrsmith/temporal-doom/web"
)

type server struct {
	c             client.Client
	namespace     string
	temporalUI    string
	leaderboard   string
	engineVersion string
	hub           *netplay.Hub
	leases        *leases // who is playing each single-player game
}

func main() {
	var f setup.Flags
	f.Register()
	listen := flag.String("listen", "localhost:8080", "HTTP listen address")
	leaderboard := flag.String("leaderboard", "data/leaderboard.jsonl", "leaderboard file written by the worker")
	temporalUI := flag.String("temporal-ui", "http://localhost:8233", "Temporal Web UI base URL, for links")
	flag.Parse()

	wasm, err := os.ReadFile(f.Engine)
	if err != nil {
		log.Fatalf("%v (run `make engine`)", err)
	}
	wad, err := os.ReadFile(f.WAD)
	if err != nil {
		log.Fatalf("%v (run `make wad`)", err)
	}
	c, err := f.Dial()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	hub := netplay.NewHub(ctx, netplay.Temporal{Client: c})
	go hub.LogStats(ctx, time.Minute)
	s := &server{c: c, namespace: f.Namespace, temporalUI: *temporalUI, leaderboard: *leaderboard, engineVersion: doomvm.VersionOf(wasm, wad), hub: hub, leases: newLeases()}
	go s.leases.sweep(ctx, time.Minute)
	mux := http.NewServeMux()
	static, _ := fs.Sub(web.FS, ".")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /doom.wasm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/wasm")
		w.Write(wasm)
	})
	mux.HandleFunc("GET /doom1.wad", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(wad)
	})
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/games", s.listGames)
	mux.HandleFunc("POST /api/games", s.startGame)
	mux.HandleFunc("POST /api/games/{id}/tics", s.tics)
	mux.HandleFunc("POST /api/games/{id}/quit", s.quit)
	mux.HandleFunc("GET /api/games/{id}/net", func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/games/{id}/frame", s.frame)
	mux.HandleFunc("GET /api/games/{id}/sync", s.query(game.QuerySync, func() any { return &game.Sync{} }))
	mux.HandleFunc("GET /api/games/{id}/state", s.state)
	mux.HandleFunc("GET /api/leaderboard", s.leaderboardHandler)

	log.Printf("DOOM on Temporal: http://%s (engine %s)", *listen, s.engineVersion)
	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// Slow clients can't hold connections open forever. WebSockets
		// clear these deadlines when they upgrade, and set their own.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// An update waits for the worker, and may be retried through a
		// continue-as-new, so leave it plenty of time.
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 16 << 10,
	}
	log.Fatal(srv.ListenAndServe())
}

// Request body limits. A TicBatch of MaxBatch tics is about 5KB of JSON.
const (
	maxStartBody = 4 << 10
	maxTicsBody  = 64 << 10
)

// readJSON decodes a request body of up to limit bytes. If it can't, it
// replies with an error and returns false.
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
	if err == nil {
		return true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request body"})
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeErr replies with an error from Temporal. Only messages the game
// wrote itself go back to the client; anything else could describe the
// server's internals, so it's logged and the client gets a generic message.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := errorReply(err)
	if code >= 500 {
		log.Printf("%s %q: %v", r.Method, r.URL.Path, err)
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func errorReply(err error) (int, string) {
	// Rejected by a validator: the client should resync or rejoin.
	for _, s := range []string{"out of sync", "game is over"} {
		if i := strings.Index(err.Error(), s); i >= 0 {
			return http.StatusConflict, err.Error()[i:]
		}
	}
	// Failed by the update handler, or rejected by a validator.
	if app, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		return http.StatusConflict, app.Message()
	}
	if _, ok := errors.AsType[*serviceerror.NotFound](err); ok {
		return http.StatusNotFound, "game not found"
	}
	if _, ok := errors.AsType[*serviceerror.InvalidArgument](err); ok {
		return http.StatusBadRequest, "invalid request"
	}
	if _, ok := errors.AsType[*serviceerror.QueryFailed](err); ok {
		return http.StatusBadRequest, "query failed"
	}
	return http.StatusInternalServerError, "internal error"
}

func (s *server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"engineVersion": s.engineVersion,
		"temporalUI":    s.temporalUI,
		"namespace":     s.namespace,
		"maxBatch":      game.MaxBatch,
	})
}

// newGame is the body of POST /api/games: the parts of a GameConfig that
// players get to choose. The rest, like how long a game can sit idle, is
// the server's to decide.
type newGame struct {
	Player  string `json:"player"`
	Skill   int    `json:"skill"`
	Episode int    `json:"episode"`
	Map     int    `json:"map"`
	Mode    string `json:"mode"`
}

func (s *server) startGame(w http.ResponseWriter, r *http.Request) {
	var req newGame
	if !readJSON(w, r, maxStartBody, &req) {
		return
	}
	cfg := game.GameConfig{Player: game.PlayerName(req.Player), Skill: req.Skill, Episode: req.Episode, Map: req.Map, Mode: req.Mode}
	if cfg.Mode != doomvm.Coop && cfg.Mode != doomvm.Deathmatch {
		cfg.Mode = doomvm.Single
	}
	if cfg.Skill < 1 || cfg.Skill > 5 {
		cfg.Skill = 3
	}
	if cfg.Episode != 1 || cfg.Map < 1 || cfg.Map > 9 {
		cfg.Episode, cfg.Map = 1, 1 // shareware
	}
	run, err := game.StartGame(r.Context(), s.c, cfg)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]string{"id": run.GetID(), "runId": run.GetRunID()})
}

type gameRow struct {
	ID      string    `json:"id"`
	Player  string    `json:"player"`
	Mode    string    `json:"mode"`
	Players []string  `json:"players"`
	Map     string    `json:"map"`
	Health  int64     `json:"health"`
	Kills   int64     `json:"kills"`
	Deaths  int64     `json:"deaths"`
	Started time.Time `json:"started"`
	// Busy is set if someone is playing a single-player game.
	Busy bool `json:"busy,omitempty"`
}

func (s *server) listGames(w http.ResponseWriter, r *http.Request) {
	query := fmt.Sprintf("WorkflowType = '%s' AND ExecutionStatus = 'Running'", game.WorkflowName)
	if q := r.URL.Query().Get("q"); q != "" {
		// e.g. DoomMap = 'E1M8' AND DoomHealth < 20
		q, err := sanitizeSearch(q)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid search: " + err.Error()})
			return
		}
		query += " AND (" + q + ")"
	}
	resp, err := s.c.ListWorkflow(r.Context(), &workflowservice.ListWorkflowExecutionsRequest{Namespace: s.namespace, Query: query, PageSize: 50})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	rows := []gameRow{}
	now := time.Now()
	for _, e := range resp.GetExecutions() {
		row := rowFor(e)
		row.Busy = s.leases.busy(row.ID, "", now)
		rows = append(rows, row)
	}
	writeJSON(w, 200, map[string]any{"query": query, "games": rows})
}

func rowFor(e *workflowpb.WorkflowExecutionInfo) gameRow {
	row := gameRow{ID: e.GetExecution().GetWorkflowId(), Started: e.GetStartTime().AsTime()}
	dc := converter.GetDefaultDataConverter()
	fields := e.GetSearchAttributes().GetIndexedFields()
	get := func(name string, v any) {
		if p, ok := fields[name]; ok {
			dc.FromPayload(p, v)
		}
	}
	get(game.SAPlayer.GetName(), &row.Player)
	get(game.SAMap.GetName(), &row.Map)
	get(game.SAHealth.GetName(), &row.Health)
	get(game.SAKills.GetName(), &row.Kills)
	get(game.SADeaths.GetName(), &row.Deaths)
	get(game.SAMode.GetName(), &row.Mode)
	get(game.SAPlayers.GetName(), &row.Players)
	return row
}

// writeBusy replies that someone else is playing the game.
func writeBusy(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": "someone else is playing this game", "code": "busy"})
}

func (s *server) tics(w http.ResponseWriter, r *http.Request) {
	id, player := r.PathValue("id"), r.Header.Get(playerHeader)
	if player == "" || len(player) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "need an " + playerHeader + " header of up to 64 characters"})
		return
	}
	var b game.TicBatch
	if !readJSON(w, r, maxTicsBody, &b) {
		return
	}
	if !s.leases.claim(id, player, time.Now()) {
		writeBusy(w)
		return
	}
	var res game.TicResult
	if err := game.SendUpdate(r.Context(), s.c, id, game.UpdateTics, b, &res); err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *server) quit(w http.ResponseWriter, r *http.Request) {
	var sum game.Summary
	id, player := r.PathValue("id"), r.Header.Get(playerHeader)
	if s.leases.busy(id, player, time.Now()) || s.hub.Busy(id, player) {
		writeBusy(w)
		return
	}
	if err := game.SendUpdate(r.Context(), s.c, id, game.UpdateQuit, nil, &sum); err != nil {
		writeErr(w, r, err)
		return
	}
	s.leases.release(id)
	s.hub.End(id, "quit")
	writeJSON(w, 200, sum)
}

func (s *server) frame(w http.ResponseWriter, r *http.Request) {
	var fr game.Frame
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	view, _ := strconv.Atoi(r.URL.Query().Get("view"))
	if err := s.queryInto(r.Context(), r.PathValue("id"), game.QueryFrame, &fr, int32(since), view); err != nil {
		writeErr(w, r, err)
		return
	}
	stats, _ := json.Marshal(fr.Stats)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Doom-Stats", string(stats))
	if len(fr.Sounds) > 0 {
		sounds, _ := json.Marshal(fr.Sounds)
		w.Header().Set("X-Doom-Sounds", string(sounds))
	}
	w.Write(fr.PNG)
}

// state serves the state Query, plus whether the game is open to the
// player asking: busy if someone else is playing a single-player game,
// full if a multiplayer game has no slot for them.
func (s *server) state(w http.ResponseWriter, r *http.Request) {
	var st game.State
	id, player := r.PathValue("id"), r.Header.Get(playerHeader)
	if err := s.queryInto(r.Context(), id, game.QueryState, &st); err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, struct {
		game.State
		Busy bool `json:"busy,omitempty"`
		Full bool `json:"full,omitempty"`
	}{st, !st.Config.Multiplayer() && s.leases.busy(id, player, time.Now()), st.Config.Multiplayer() && s.hub.Full(id, player)})
}

func (s *server) queryInto(ctx context.Context, id, name string, out any, args ...any) error {
	v, err := s.c.QueryWorkflow(ctx, id, "", name, args...)
	if err != nil {
		return err
	}
	return v.Get(out)
}

func (s *server) query(name string, newResult func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := newResult()
		if err := s.queryInto(r.Context(), r.PathValue("id"), name, out); err != nil {
			writeErr(w, r, err)
			return
		}
		writeJSON(w, 200, out)
	}
}

func (s *server) leaderboardHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := game.ReadLeaderboard(s.leaderboard)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if rows == nil {
		rows = []game.LevelResult{}
	}
	writeJSON(w, 200, rows)
}

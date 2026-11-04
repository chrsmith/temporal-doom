// Command replay turns a DOOM workflow's history back into gameplay.
//
// It fetches the history of every run of a game (following the
// continue-as-new chain) and feeds it to Temporal's WorkflowReplayer: the
// exact mechanism a worker uses to recover a workflow after a crash. Along
// the way it renders frames into an animated GIF (or a directory of PNGs),
// and checks every checksum recorded in history against the one the replay
// computes. A mismatch is a demo desync: the code or engine changed in a way
// that changes the game.
//
//	go run ./cmd/replay -id doom-bot-1234 -out game.gif
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/gif"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/chrsmith/temporal-doom/internal/doomvm"
	"github.com/chrsmith/temporal-doom/internal/game"
	"github.com/chrsmith/temporal-doom/internal/setup"
)

func main() {
	var f setup.Flags
	f.Register()
	id := flag.String("id", "", "workflow ID of the game")
	out := flag.String("out", "", "output: a .gif file, or a directory for PNG frames (e.g. for ffmpeg)")
	every := flag.Int("every", 2, "render a frame every N tics")
	fromTic := flag.Int("from", 0, "first tic to render")
	toTic := flag.Int("to", -1, "last tic to render (-1 = end)")
	historyFile := flag.String("history", "", "replay a single run from a history JSON file (temporal workflow show -o json) instead of fetching")
	flag.Parse()
	if *id == "" && *historyFile == "" {
		log.Fatal("-id or -history is required")
	}
	ctx := context.Background()

	rt, err := f.Runtime(ctx)
	if err != nil {
		log.Fatal(err)
	}

	var runs []*historypb.History
	if *historyFile != "" {
		data, err := os.ReadFile(*historyFile)
		if err != nil {
			log.Fatal(err)
		}
		h, err := client.HistoryFromJSON(strings.NewReader(string(data)), client.HistoryJSONOptions{})
		if err != nil {
			log.Fatal(err)
		}
		runs = append(runs, h)
	} else {
		c, err := f.Dial()
		if err != nil {
			log.Fatal(err)
		}
		defer c.Close()
		if runs, err = fetchRuns(ctx, c, *id); err != nil {
			log.Fatal(err)
		}
	}

	rec := &recorder{every: *every, from: int32(*fromTic), to: int32(*toTic), out: *out}
	desyncs := 0
	for i, h := range runs {
		rec.expected = recordedResults(h)
		rec.batch = 0
		wfs := &game.Workflows{Engine: rt, Observer: rec, RenderEvery: *every}
		replayer := worker.NewWorkflowReplayer()
		replayer.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: game.WorkflowName})
		logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
		start := time.Now()
		err := replayer.ReplayWorkflowHistory(logger, h)
		fmt.Printf("run %d: %d events, %d updates replayed in %v, checksums %s\n", i, len(h.Events), rec.batch, time.Since(start).Round(time.Millisecond), verdict(rec.desyncs))
		if err != nil {
			fmt.Printf("  replay failed: %v\n", err)
			desyncs++
		}
		desyncs += rec.desyncs
		rec.desyncs = 0
	}

	if *out != "" {
		if err := rec.write(); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %d frames to %s\n", rec.frames, *out)
	}
	if desyncs > 0 {
		fmt.Println("DESYNC: this history does not replay to the same game")
		os.Exit(1)
	}
	fmt.Printf("final: %s tic %d health=%d kills=%d/%d\n", rec.last.MapName(), rec.last.Tic, rec.last.Health, rec.last.Kills, rec.last.TotalKills)
}

func verdict(n int) string {
	if n == 0 {
		return "OK"
	}
	return fmt.Sprintf("%d MISMATCHED", n)
}

// fetchRuns returns the histories of every run of a workflow, oldest first.
func fetchRuns(ctx context.Context, c client.Client, id string) ([]*historypb.History, error) {
	desc, err := c.DescribeWorkflowExecution(ctx, id, "")
	if err != nil {
		return nil, err
	}
	runID := desc.GetWorkflowExecutionInfo().GetExecution().GetRunId()
	var runs []*historypb.History
	for runID != "" {
		h := &historypb.History{}
		it := c.GetWorkflowHistory(ctx, id, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for it.HasNext() {
			e, err := it.Next()
			if err != nil {
				return nil, err
			}
			h.Events = append(h.Events, e)
		}
		if len(h.Events) == 0 {
			return nil, errors.New("empty history")
		}
		runs = append([]*historypb.History{h}, runs...)
		runID = h.Events[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()
	}
	return runs, nil
}

// recordedResults extracts the results of successful "tics" (or, in
// multiplayer, "record") updates from a history, in order.
func recordedResults(h *historypb.History) []game.TicResult {
	names := map[string]string{}
	var results []game.TicResult
	dc := converter.GetDefaultDataConverter()
	for _, e := range h.Events {
		if a := e.GetWorkflowExecutionUpdateAcceptedEventAttributes(); a != nil {
			names[a.GetProtocolInstanceId()] = a.GetAcceptedRequest().GetInput().GetName()
		}
		if a := e.GetWorkflowExecutionUpdateCompletedEventAttributes(); a != nil {
			if name := names[a.GetMeta().GetUpdateId()]; (name != game.UpdateTics && name != game.UpdateRecord) || a.GetOutcome().GetSuccess() == nil {
				continue
			}
			var r game.TicResult
			if err := dc.FromPayloads(a.GetOutcome().GetSuccess(), &r); err == nil {
				results = append(results, r)
			}
		}
	}
	return results
}

type recorder struct {
	every    int
	from, to int32
	out      string

	expected []game.TicResult
	batch    int
	desyncs  int
	last     doomvm.Stats

	frames int
	images []*image.Paletted
	delays []int
	carry  int
}

func (r *recorder) Frame(s doomvm.Stats, pixels, palette []byte) {
	if r.out == "" || s.Tic < r.from || (r.to >= 0 && s.Tic > r.to) {
		return
	}
	r.frames++
	img := game.FrameImage(pixels, palette)
	if strings.HasSuffix(r.out, ".gif") {
		// GIF delays are in 1/100s; DOOM runs at 35 tics per second.
		r.carry += r.every * 100
		d := r.carry / 35
		r.carry -= d * 35
		r.images = append(r.images, img)
		r.delays = append(r.delays, d)
		return
	}
	if err := os.MkdirAll(r.out, 0o755); err != nil {
		log.Fatal(err)
	}
	b, err := game.EncodePNG(pixels, palette)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.out, fmt.Sprintf("frame%06d.png", s.Tic)), b, 0o644); err != nil {
		log.Fatal(err)
	}
}

func (r *recorder) Batch(res game.TicResult) {
	r.last = res.Stats
	if r.batch < len(r.expected) {
		want := r.expected[r.batch]
		if want.Stats.Tic != res.Stats.Tic || want.Stats.Checksum != res.Stats.Checksum {
			if r.desyncs == 0 {
				fmt.Printf("  desync at update %d: history says tic %d checksum %08x, replay computed tic %d checksum %08x\n",
					r.batch+1, want.Stats.Tic, want.Stats.Checksum, res.Stats.Tic, res.Stats.Checksum)
			}
			r.desyncs++
		}
	}
	r.batch++
}

func (r *recorder) write() error {
	if !strings.HasSuffix(r.out, ".gif") {
		return nil
	}
	f, err := os.Create(r.out)
	if err != nil {
		return err
	}
	defer f.Close()
	return gif.EncodeAll(f, &gif.GIF{Image: r.images, Delay: r.delays})
}

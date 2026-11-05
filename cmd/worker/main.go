// Command worker runs DOOM workflows.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/temporal-community/temporal-doom/internal/game"
	"github.com/temporal-community/temporal-doom/internal/setup"
)

func main() {
	var f setup.Flags
	f.Register()
	leaderboard := flag.String("leaderboard", "data/leaderboard.jsonl", "leaderboard file")
	flag.Parse()
	ctx := context.Background()

	rt, err := f.Runtime(ctx)
	if err != nil {
		log.Fatalf("loading engine: %v", err)
	}
	log.Printf("DOOM engine version %s", rt.Version)

	c, err := f.Dial()
	if err != nil {
		log.Fatalf("connecting to Temporal: %v", err)
	}
	defer c.Close()
	if err := setup.EnsureSearchAttributes(ctx, c, f.Namespace); err != nil {
		log.Printf("warning: couldn't register search attributes: %v", err)
	}

	w := worker.New(c, game.TaskQueue, worker.Options{
		// A game's workflow is cached on the worker that last ran it, and
		// its tasks go to that worker's "sticky" queue. If the worker dies,
		// Temporal waits this long before handing the task to any worker
		// (which rebuilds the game by replaying history). The default is
		// 5s; a game wants to fail over faster, and replay is cheap.
		StickyScheduleToStartTimeout: time.Second,
	})
	wfs := &game.Workflows{Engine: rt}
	w.RegisterWorkflowWithOptions(wfs.DoomGame, workflow.RegisterOptions{Name: game.WorkflowName})
	w.RegisterActivity(&game.Activities{LeaderboardPath: *leaderboard})

	log.Printf("worker polling task queue %q on %s", game.TaskQueue, f.Address)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatal(err)
	}
}

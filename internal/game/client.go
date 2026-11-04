package game

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// StartOptions are the options for starting a game.
func StartOptions(id string, cfg GameConfig) client.StartWorkflowOptions {
	sa := []temporal.SearchAttributeUpdate{
		SAPlayer.ValueSet(cfg.Player),
		SAMap.ValueSet(cfg.options(nil).MapName()),
	}
	if cfg.Multiplayer() {
		sa = append(sa, SAMode.ValueSet(cfg.Mode), SAPlayers.ValueSet([]string{}))
	}
	return client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             TaskQueue,
		TypedSearchAttributes: temporal.NewSearchAttributes(sa...),
		// If a worker dies while processing a workflow task, Temporal
		// retries the task elsewhere after this timeout. The default is 10s,
		// which is a long time to stare at a frozen imp. Our tasks take
		// milliseconds, and even replaying a whole run takes well under a
		// second.
		WorkflowTaskTimeout: 2 * time.Second,
	}
}

// StartGame starts a game under a new random workflow ID.
//
// By default, starting a workflow whose ID is already running returns that
// workflow instead, which would put a new player in someone else's game.
// So an ID that's in use, now or ever before, is an error, and StartGame
// tries another.
func StartGame(ctx context.Context, c client.Client, cfg GameConfig) (client.WorkflowRun, error) {
	var err error
	for range 3 {
		id := fmt.Sprintf("doom-%s-%08x", strings.ToLower(cfg.Player), rand.Uint32())
		opts := StartOptions(id, cfg)
		opts.WorkflowExecutionErrorWhenAlreadyStarted = true
		opts.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
		var run client.WorkflowRun
		run, err = c.ExecuteWorkflow(ctx, opts, WorkflowName, GameInput{Config: cfg})
		if _, taken := errors.AsType[*serviceerror.WorkflowExecutionAlreadyStarted](err); !taken {
			return run, err
		}
	}
	return nil, err
}

// SendUpdate sends an update to the game's current run and waits for the
// result.
//
// Updates are addressed by workflow ID only, so they always go to the latest
// run. If one races with a continue-as-new (it reaches the old run just as
// it closes) the server fails it without applying it, and it's safe to
// simply send it again: it will land on the new run.
func SendUpdate(ctx context.Context, c client.Client, workflowID, name string, arg, result any) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		var h client.WorkflowUpdateHandle
		opts := client.UpdateWorkflowOptions{
			WorkflowID:   workflowID,
			UpdateName:   name,
			WaitForStage: client.WorkflowUpdateStageCompleted,
		}
		if arg != nil {
			opts.Args = []any{arg}
		}
		h, err = c.UpdateWorkflow(ctx, opts)
		if err == nil {
			err = h.Get(ctx, result)
		}
		if err == nil || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return err
}

func retryable(err error) bool {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		return false // rejected by the validator or failed by the handler
	}
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"already completed", "closing", "aborted", "continued as new", "Workflow Update was rejected because workflow is closing"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

var namePattern = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// PlayerName makes a player-supplied name safe to show and to use in a
// workflow ID.
func PlayerName(name string) string {
	name = namePattern.ReplaceAllString(name, "")
	if name == "" {
		name = "marine"
	}
	return name[:min(len(name), MaxName)]
}

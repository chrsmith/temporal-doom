// Package setup holds flag and connection boilerplate shared by the commands.
package setup

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/operatorservice/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"

	"github.com/temporal-community/temporal-doom/internal/doomvm"
	"github.com/temporal-community/temporal-doom/internal/game"
)

// Flags are the common command line flags.
type Flags struct {
	Address   string
	Namespace string
	Engine    string
	WAD       string
}

// Register adds the common flags to the default flag set.
func (f *Flags) Register() {
	flag.StringVar(&f.Address, "address", envOr("TEMPORAL_ADDRESS", client.DefaultHostPort), "Temporal frontend address")
	flag.StringVar(&f.Namespace, "namespace", envOr("TEMPORAL_NAMESPACE", client.DefaultNamespace), "Temporal namespace")
	flag.StringVar(&f.Engine, "engine", "build/doom.wasm", "path to the DOOM engine WASM module")
	flag.StringVar(&f.WAD, "wad", "wad/doom1.wad", "path to the DOOM IWAD")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Dial connects to Temporal.
//
// For Temporal Cloud, set TEMPORAL_API_KEY, or TEMPORAL_TLS_CERT and
// TEMPORAL_TLS_KEY (paths to a client certificate and key) for mTLS. Either
// one turns on TLS.
func (f *Flags) Dial() (client.Client, error) {
	opts := client.Options{
		HostPort:  f.Address,
		Namespace: f.Namespace,
		Logger:    tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
	if key := os.Getenv("TEMPORAL_API_KEY"); key != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(key)
		opts.ConnectionOptions.TLS = &tls.Config{}
	}
	if certFile, keyFile := os.Getenv("TEMPORAL_TLS_CERT"), os.Getenv("TEMPORAL_TLS_KEY"); certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("loading Temporal client certificate: %w", err)
		}
		opts.ConnectionOptions.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	return client.Dial(opts)
}

// Runtime loads and compiles the engine.
func (f *Flags) Runtime(ctx context.Context) (*doomvm.Runtime, error) {
	wasm, err := os.ReadFile(f.Engine)
	if err != nil {
		return nil, fmt.Errorf("%w (run `make engine`)", err)
	}
	wad, err := os.ReadFile(f.WAD)
	if err != nil {
		return nil, fmt.Errorf("%w (run `make wad`)", err)
	}
	cache := filepath.Join(os.TempDir(), "temporal-doom-wazero-cache")
	return doomvm.NewRuntime(ctx, wasm, wad, cache)
}

// EnsureSearchAttributes registers the game's custom search attributes.
func EnsureSearchAttributes(ctx context.Context, c client.Client, namespace string) error {
	want := map[string]enumspb.IndexedValueType{
		game.SAPlayer.GetName():  enumspb.INDEXED_VALUE_TYPE_KEYWORD,
		game.SAMap.GetName():     enumspb.INDEXED_VALUE_TYPE_KEYWORD,
		game.SAHealth.GetName():  enumspb.INDEXED_VALUE_TYPE_INT,
		game.SAKills.GetName():   enumspb.INDEXED_VALUE_TYPE_INT,
		game.SADeaths.GetName():  enumspb.INDEXED_VALUE_TYPE_INT,
		game.SAMode.GetName():    enumspb.INDEXED_VALUE_TYPE_KEYWORD,
		game.SAPlayers.GetName(): enumspb.INDEXED_VALUE_TYPE_KEYWORD_LIST,
	}
	existing, err := c.OperatorService().ListSearchAttributes(ctx, &operatorservice.ListSearchAttributesRequest{Namespace: namespace})
	if err != nil {
		return err
	}
	for name := range existing.GetCustomAttributes() {
		delete(want, name)
	}
	if len(want) == 0 {
		return nil
	}
	_, err = c.OperatorService().AddSearchAttributes(ctx, &operatorservice.AddSearchAttributesRequest{
		Namespace:        namespace,
		SearchAttributes: want,
	})
	var exists *serviceerror.AlreadyExists
	if errors.As(err, &exists) {
		return nil
	}
	return err
}

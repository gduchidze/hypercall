// hypercall-evaluator executes independently leased evaluation work. It owns no
// sandbox lifecycle and uses only operator-configured isolated scorer artifacts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/eval"
	"github.com/gduchidze/hypercall/internal/scorer"
)

func main() {
	if err := run(); err != nil {
		slog.Error("evaluation worker stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	configFile := flag.String("scorers-file", "", "private JSON array of pinned scorer.Config records (Timeout in nanoseconds)")
	drain := flag.Bool("drain", false, "finish currently eligible work then exit")
	recoverOwner := flag.String("recover-fenced-owner", "", "remove abandoned scorer workers for an operator-confirmed fenced old owner, then exit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn := os.Getenv("HYPERCALL_DATABASE_URL")
	if dsn == "" {
		return errors.New("HYPERCALL_DATABASE_URL required")
	}
	store, err := cp.Open(ctx, dsn)
	if err != nil {
		return errors.New("database unavailable")
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		return err
	}
	owner := fmt.Sprintf("eval-%d-%d", os.Getpid(), time.Now().UnixNano())
	graders := eval.IsolatedScorer{}
	if *configFile != "" {
		info, err := os.Stat(*configFile)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("scorer configuration must be private regular file (0600)")
		}
		data, err := os.ReadFile(*configFile)
		if err != nil {
			return err
		}
		var configs []scorer.Config
		if err = json.Unmarshal(data, &configs); err != nil {
			return err
		}
		for _, config := range configs {
			if _, exists := graders[config.CodeDigest]; exists {
				return errors.New("duplicate scorer digest")
			}
			config.OwnerID = owner
			if *recoverOwner != "" {
				config.OwnerID = *recoverOwner
			}
			runner := scorer.NewRunner(config)
			graders[config.CodeDigest] = runner
			if *recoverOwner != "" {
				if err = runner.Recover(ctx); err != nil {
					return err
				}
			}
		}
	}
	if *recoverOwner != "" {
		if len(graders) == 0 {
			return errors.New("recovery requires pinned --scorers-file")
		}
		return nil
	}
	service := cp.NewService(store, cp.UnavailableBackend{})
	slog.Info("evaluation worker ready", "owner", owner, "scorers", len(graders))
	for ctx.Err() == nil {
		err = service.WorkEvaluation(ctx, owner, 30*time.Second, graders)
		if cp.Code(err) == "not_found" {
			if *drain {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("evaluation attempt failed; retained lease will recover", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

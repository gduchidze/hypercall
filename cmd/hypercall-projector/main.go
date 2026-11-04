// hypercall-projector runs the independent, rebuildable analytics consumer.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/analytics"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("projector stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	endpoint := flag.String("clickhouse-url", os.Getenv("HYPERCALL_CLICKHOUSE_URL"), "ClickHouse HTTPS origin")
	database := flag.String("clickhouse-database", os.Getenv("HYPERCALL_CLICKHOUSE_DATABASE"), "existing ClickHouse database with schema installed")
	allowLocal := flag.Bool("allow-insecure-loopback", os.Getenv("HYPERCALL_CLICKHOUSE_ALLOW_INSECURE_LOOPBACK") == "true", "allow HTTP only to an explicit loopback IP for local development")
	rebuild := flag.Bool("rebuild", false, "reset only clickhouse-v1 receipts, then replay retained sources")
	drain := flag.Bool("drain", false, "exit after currently available work is projected; fail on sink error")
	poll := flag.Duration("poll-interval", time.Second, "idle polling interval (10ms..1m)")
	statusTenant := flag.String("status-tenant", "", "print lag for this tenant with --status-project, then exit")
	statusProject := flag.String("status-project", "", "project paired with --status-tenant")
	flag.Parse()
	if *poll < 10*time.Millisecond || *poll > time.Minute {
		return errors.New("poll interval must be 10ms..1m")
	}
	if (*statusTenant == "") != (*statusProject == "") {
		return errors.New("status requires both tenant and project")
	}
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	dsn := os.Getenv("HYPERCALL_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("HYPERCALL_POSTGRES_DSN is required")
	}
	store, err := cp.Open(ctx, dsn)
	if err != nil {
		return errors.New("connect to Postgres failed")
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate Postgres: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("open projector database failed")
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	// Status is Postgres-derived and does not need ClickHouse credentials or access.
	if *statusTenant != "" {
		status, err := analytics.NewProjector(db, nil).Status(ctx, cp.Scope{TenantID: *statusTenant, ProjectID: *statusProject})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	sink, err := analytics.NewClickHouse(analytics.ClickHouseConfig{Endpoint: *endpoint, Database: *database, Username: os.Getenv("HYPERCALL_CLICKHOUSE_USER"), Password: os.Getenv("HYPERCALL_CLICKHOUSE_PASSWORD"), AllowInsecureLoopback: *allowLocal})
	if err != nil {
		return err
	}
	projector := analytics.NewProjector(db, sink)
	if *rebuild {
		if err = projector.Rebuild(ctx); err != nil {
			return fmt.Errorf("reset projection receipts: %w", err)
		}
		slog.Info("projection replay requested", "consumer", analytics.Consumer)
	}
	slog.Info("projector started", "consumer", analytics.Consumer)
	backoff := *poll
	for {
		worked, err := projector.ProjectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if *drain {
				return err
			}
			slog.Warn("projection pending after failed delivery", "consumer", analytics.Consumer, "error", err, "retry_after", backoff)
			if !pause(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = *poll
		if worked {
			continue
		}
		if *drain {
			return nil
		}
		if !pause(ctx, *poll) {
			return nil
		}
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

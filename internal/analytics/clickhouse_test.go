package analytics_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/analytics"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
)

func clickhouse(t *testing.T) *analytics.ClickHouse {
	t.Helper()
	endpoint := os.Getenv("HYPERCALL_TEST_CLICKHOUSE_URL")
	if endpoint == "" {
		t.Skip("HYPERCALL_TEST_CLICKHOUSE_URL is not set")
	}
	sink, err := analytics.NewClickHouse(analytics.ClickHouseConfig{Endpoint: endpoint, Database: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_DATABASE"), Username: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_USER"), Password: os.Getenv("HYPERCALL_TEST_CLICKHOUSE_PASSWORD"), AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func TestClickHouseRetriesAreLogicallyDeduplicated(t *testing.T) {
	sink := clickhouse(t)
	scope := cp.Scope{TenantID: "projection-retry-" + time.Now().Format("150405.000000000"), ProjectID: "project"}
	event := analytics.Event{EventID: "branch:retry", TenantID: scope.TenantID, ProjectID: scope.ProjectID, Kind: "step", ResourceID: "branch-one", OccurredAt: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), PayloadJSON: string(json.RawMessage(`{"output":"quoted ' \\ unicode λ"}`)), SchemaVersion: 1}
	for range 2 {
		if err := sink.Write(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := sink.Events(context.Background(), scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PayloadJSON != event.PayloadJSON || got[0].EventID != "branch:retry" {
		t.Fatalf("logical retry projection: %+v", got)
	}
	other, err := sink.Events(context.Background(), cp.Scope{TenantID: "another-tenant", ProjectID: scope.ProjectID}, 10)
	if err != nil || len(other) != 0 {
		t.Fatalf("tenant isolation: %+v, %v", other, err)
	}
}

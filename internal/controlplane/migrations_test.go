package controlplane_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"os"
	"sync"
	"testing"
)

func TestMigrationConcurrentFirstApplicationAndRerun(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	errs := make(chan error, 4)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			s, e := cp.Open(ctx, dsn)
			if e != nil {
				errs <- e
				return
			}
			defer s.Close()
			errs <- s.Migrate(ctx)
		}()
	}
	workers.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	s, e := cp.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		t.Fatalf("idempotent rerun: %v", e)
	}
}
func TestMigrationRejectsChecksumDriftAndFutureVersion(t *testing.T) {
	for _, fault := range []string{"UPDATE schema_migrations SET checksum='changed' WHERE version=1", "INSERT INTO schema_migrations VALUES(99,'future')"} {
		t.Run(fault[:6], func(t *testing.T) {
			s, _, _ := fixture(t)
			db, e := sql.Open("pgx", testDSN(t))
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			if _, e = db.Exec(fault); e != nil {
				t.Fatal(e)
			}
			if e = s.Migrate(context.Background()); cp.Code(e) != "failed_precondition" {
				t.Fatalf("incompatible migration accepted: %v", e)
			}
		})
	}
}

func TestMigrationUpgradesPinnedV1WithoutChangingRetainedResources(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	v1, e := os.ReadFile("migrations/001_initial.sql")
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(v1)
	checksum := hex.EncodeToString(sum[:])
	if checksum != "88fd17690992f7d0cd4fd9125e113d63aa344e6dd29610f592b2fb3d925a23d9" {
		t.Fatal("pinned 001 migration changed")
	}
	if _, e = db.ExecContext(ctx, string(v1)); e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(ctx, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL)`); e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(1,$1)`, checksum); e != nil {
		t.Fatal(e)
	}
	st, e := cp.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc := cp.NewService(st, cp.UnavailableBackend{})
	scope := cp.Scope{TenantID: "legacy", ProjectID: "project"}
	resource, e := svc.PutResource(ctx, scope, cp.Resource{Kind: "Gateway", Metadata: cp.Metadata{Name: "legacy-gateway"}, Spec: json.RawMessage(`{"routes":{"fixture":"https://fixture.invalid"}}`)}, "legacy-put")
	if e != nil {
		t.Fatal(e)
	}
	if e = st.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = st.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	retained, e := svc.GetResource(ctx, scope, "Gateway", resource.Metadata.ID)
	if e != nil || retained.Metadata.Digest != resource.Metadata.Digest {
		t.Fatalf("upgrade changed retained resource: %v", e)
	}
	replayed, e := svc.PutResource(ctx, scope, cp.Resource{Kind: "Gateway", Metadata: cp.Metadata{Name: "legacy-gateway"}, Spec: json.RawMessage(`{"routes":{"fixture":"https://fixture.invalid"}}`)}, "legacy-put")
	if e != nil || replayed.Metadata.ID != resource.Metadata.ID {
		t.Fatalf("upgrade lost idempotency: %v", e)
	}
}

func TestMigrationV2RetainsLegacyPhysicalAllocationIdentity(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.ExecContext(ctx, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL)`); e != nil {
		t.Fatal(e)
	}
	pins := []string{"88fd17690992f7d0cd4fd9125e113d63aa344e6dd29610f592b2fb3d925a23d9", "265b46260a49e1bf2af425c8e3cfd2d35ab36f89c8290f2dca39b5987cfcd720"}
	for i, name := range []string{"001_initial.sql", "002_resume.sql"} {
		raw, e := os.ReadFile("migrations/" + name)
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != pins[i] {
			t.Fatalf("pinned %s changed", name)
		}
		if _, e = db.ExecContext(ctx, string(raw)); e != nil {
			t.Fatal(e)
		}
		if _, e = db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES($1,$2)`, i+1, pins[i]); e != nil {
			t.Fatal(e)
		}
	}
	scope := cp.Scope{TenantID: "legacy", ProjectID: "project"}
	if _, e = db.ExecContext(ctx, `INSERT INTO projects VALUES($1,$2)`, scope.TenantID, scope.ProjectID); e != nil {
		t.Fatal(e)
	}
	branch := cp.Resource{Kind: "Branch", Metadata: cp.Metadata{ID: "legacy-branch", TenantID: scope.TenantID, ProjectID: scope.ProjectID, Name: "legacy-branch"}, Spec: json.RawMessage(`{"run_id":"legacy-run"}`)}
	body, _ := json.Marshal(branch)
	if _, e = db.ExecContext(ctx, `INSERT INTO resources VALUES($1,$2,'Branch',$3,$3,$4)`, scope.TenantID, scope.ProjectID, branch.Metadata.ID, body); e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(ctx, `INSERT INTO branches(tenant_id,project_id,id,desired,observed,execution_id,epoch,lease_until) VALUES($1,$2,$3,'running','running','legacy-execution',7,clock_timestamp()+interval '1 minute')`, scope.TenantID, scope.ProjectID, branch.Metadata.ID); e != nil {
		t.Fatal(e)
	}
	st, e := cp.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if e = st.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	svc := cp.NewService(st, cp.UnavailableBackend{})
	status := statusOf(t, svc, scope, branch.Metadata.ID)
	if status.SandboxID != branch.Metadata.ID || status.ExecutionID != "legacy-execution" || status.Epoch != 7 || status.Observed != "running" {
		t.Fatalf("upgrade rebound retained allocation identity: %+v", status)
	}
	if e = st.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
}

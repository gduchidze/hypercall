package harness_test

import (
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/harness"
	"testing"
)

func TestSessionSealBindsFreshExecutionAndRouting(t *testing.T) {
	h := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s, e := harness.SealSession(&harnessv1.HarnessSession{TenantId: "tenant", ProjectId: "project", BranchId: "branch", SandboxId: "sandbox", ExecutionId: "execution", Epoch: 1, CallNamespace: "calls", RouteDigest: h, ModelDigest: h, ProtocolVersion: "hypercall.harness.v1"})
	if e != nil {
		t.Fatal(e)
	}
	if e = harness.ValidateSession(s); e != nil {
		t.Fatal(e)
	}
	s.ExecutionId = "other"
	if e = harness.ValidateSession(s); e == nil {
		t.Fatal("inherited execution identity kept old routing authority")
	}
}
func TestMessageHashUsesDeterministicProtobufBytes(t *testing.T) {
	got, e := harness.MessageHash(&harnessv1.AgentRequest{})
	if e != nil || got != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty protobuf hash %s: %v", got, e)
	}
}

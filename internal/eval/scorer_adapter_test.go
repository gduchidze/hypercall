package eval_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/eval"
	"github.com/gduchidze/hypercall/internal/scorer"
)

func TestIsolatedScorerPreservesFailureCodeAndReason(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	adapter := eval.IsolatedScorer{digest: scorer.NewRunner(scorer.Config{})}
	result, err := adapter.Score(context.Background(), eval.Assertion{Type: "python", ScorerDigest: digest}, "output", nil)
	if err == nil || result.Pass || result.ErrorCode != "invalid_configuration" || !strings.Contains(result.Reason, "pinned image") || !strings.Contains(err.Error(), "pinned image") || result.Cost != nil {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func adapterConfig(t *testing.T, code string) scorer.Config {
	t.Helper()
	image := os.Getenv("HYPERCALL_SCORER_IMAGE")
	if image == "" {
		t.Skip("set HYPERCALL_SCORER_IMAGE for real OCI adapter tests")
	}
	path := filepath.Join(t.TempDir(), "scorer.py")
	if err := os.WriteFile(path, []byte(code), 0644); err != nil {
		t.Fatal(err)
	}
	return scorer.Config{ImageDigest: image, CodePath: path, CodeDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(code))), OwnerID: fmt.Sprintf("adapter-%d", time.Now().UnixNano()), CPU: 0.5, MemoryMB: 128, PIDs: 64, Timeout: 10 * time.Second}
}

func TestIsolatedScorerPreservesRuntimeErrorCategories(t *testing.T) {
	for _, fixture := range []struct {
		name, code, want string
		timeout          time.Duration
	}{
		{"timeout", "import time\ndef score(request):\n    time.sleep(300)\n", "deadline_exceeded", time.Second},
		{"malformed", "def score(request):\n    return {'pass': True}\n", "malformed_result", 10 * time.Second},
		{"worker", "import os\ndef score(request):\n    os._exit(7)\n", "worker_error", 10 * time.Second},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			config := adapterConfig(t, fixture.code)
			config.Timeout = fixture.timeout
			result, err := (eval.IsolatedScorer{config.CodeDigest: scorer.NewRunner(config)}).Score(context.Background(), eval.Assertion{Type: "python", ScorerDigest: config.CodeDigest}, "output", nil)
			if err == nil || result.Pass || result.ErrorCode != fixture.want || result.Reason == "" || !strings.Contains(err.Error(), fixture.want) || result.Cost != nil {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestIsolatedScorerPreservesKnownCostWhenCleanupFails(t *testing.T) {
	config := adapterConfig(t, "def score(request):\n    return {'pass': True, 'score': 1, 'reason':'synthetic fixture', 'cost': {'amount':0.0002,'currency':'USD','kind':'estimated','source':'synthetic-price-v1'}}\n")
	realConfig := config
	t.Cleanup(func() { _ = scorer.NewRunner(realConfig).Recover(context.Background()) })
	// Docker is an external system boundary: retain real container/RPC behavior,
	// injecting only a daemon cleanup failure to verify retained cost/error data.
	wrapper := filepath.Join(t.TempDir(), "docker-cleanup-failure")
	script := "#!/bin/sh\nif [ \"$1\" = rm ]; then echo 'injected daemon cleanup failure' >&2; exit 1; fi\nexec docker \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config.DockerBinary = wrapper
	result, err := (eval.IsolatedScorer{config.CodeDigest: scorer.NewRunner(config)}).Score(context.Background(), eval.Assertion{Type: "python", ScorerDigest: config.CodeDigest}, "output", nil)
	if err == nil || result.Pass || result.ErrorCode != "cleanup_error" || !strings.Contains(result.Reason, "cleanup") || !strings.Contains(err.Error(), "cleanup") || result.Cost == nil || result.Cost.Amount != 0.0002 || result.Cost.Kind != "estimated" || result.Cost.Source != "synthetic-price-v1" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestIsolatedScorerPreservesSyntheticCost(t *testing.T) {
	config := adapterConfig(t, "def score(request):\n    return {'pass':True,'score':1,'cost':{'amount':0,'currency':'USD','kind':'synthetic','source':'fixture-price-v1'}}\n")
	result, err := (eval.IsolatedScorer{config.CodeDigest: scorer.NewRunner(config)}).Score(context.Background(), eval.Assertion{Type: "python", ScorerDigest: config.CodeDigest}, "output", nil)
	if err != nil || !result.Pass || result.Cost == nil || result.Cost.Amount != 0 || result.Cost.Kind != "synthetic" || result.Cost.Source != "fixture-price-v1" {
		t.Fatalf("synthetic cost=%+v error=%v", result, err)
	}
}

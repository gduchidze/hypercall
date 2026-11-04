package scorer_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/scorer"
)

func scorerConfig(t *testing.T, code string) scorer.Config {
	t.Helper()
	image := os.Getenv("HYPERCALL_SCORER_IMAGE")
	if image == "" {
		t.Skip("set HYPERCALL_SCORER_IMAGE to immutable locally built scorer image sha256 ID")
	}
	path := filepath.Join(t.TempDir(), "scorer.py")
	if err := os.WriteFile(path, []byte(code), 0644); err != nil {
		t.Fatal(err)
	}
	return scorer.Config{ImageDigest: image, CodePath: path, CodeDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(code))), OwnerID: fmt.Sprintf("test-%d", time.Now().UnixNano()), CPU: 0.5, MemoryMB: 128, PIDs: 64, Timeout: 20 * time.Second}
}

func TestPinnedPythonScorerRunsInsideIsolation(t *testing.T) {
	config := scorerConfig(t, `import os, socket

def score(request):
    assert os.getuid() == 65532, "uid"
    assert os.environ.get("HOME") != "/root", "home"
    try:
        open("/unexpected-write", "w")
        raise AssertionError("root filesystem writable")
    except OSError:
        pass
    try:
        open("/artifact/scorer.py", "w")
        raise AssertionError("artifact writable")
    except OSError:
        pass
    outbound = socket.socket()
    outbound.settimeout(0.2)
    try:
        outbound.connect(("192.0.2.1", 443))
        raise AssertionError("outbound network accessible")
    except OSError:
        pass
    assert len(open("/proc/net/route").read().splitlines()) == 1, "outbound routes"
    assert open("/sys/fs/cgroup/memory.max").read().strip() == "134217728", "memory"
    assert open("/sys/fs/cgroup/pids.max").read().strip() == "64", "pids"
    assert open("/sys/fs/cgroup/cpu.max").read().strip() == "50000 100000", "cpu"
    assert "NoNewPrivs:\t1" in open("/proc/self/status").read(), "new privileges"
    assert "CapEff:\t0000000000000000" in open("/proc/self/status").read(), "capabilities"
    return {"pass": request["output"] == "approved", "score": 1.0, "reason": "isolated"}
`)
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{Output: "approved", ScorerDigest: config.CodeDigest})
	if err != nil || !result.Pass || result.Score != 1 || result.Reason != "isolated" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestMissingWorkerResultCannotPass(t *testing.T) {
	config := scorerConfig(t, "import os\ndef score(request):\n    os._exit(7)\n")
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
	if err == nil || result.Pass || result.ErrorCode != "worker_error" {
		t.Fatalf("missing result=%+v error=%v", result, err)
	}
}

func TestScorerDeadlineTerminatesDescendantsAndCleansWorker(t *testing.T) {
	config := scorerConfig(t, "import subprocess, time\ndef score(request):\n    subprocess.Popen(['python', '-c', 'import time; time.sleep(300)'])\n    time.sleep(300)\n")
	config.Timeout = 2 * time.Second
	start := time.Now()
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
	if err == nil || result.Pass || result.ErrorCode != "deadline_exceeded" || time.Since(start) > 10*time.Second {
		t.Fatalf("deadline result=%+v error=%v duration=%v", result, err, time.Since(start))
	}
	assertNoWorkers(t, config.OwnerID)
}

func TestMalformedScorerResultsCannotPass(t *testing.T) {
	for name, code := range map[string]string{
		"missing":      "def score(request):\n    return {'score': 1}\n",
		"nan":          "def score(request):\n    return {'pass': True, 'score': float('nan')}\n",
		"out-of-range": "def score(request):\n    return {'pass': True, 'score': 2}\n",
		"wrong-type":   "def score(request):\n    return {'pass': 'yes', 'score': 1}\n",
		"oversized":    "def score(request):\n    return {'pass': True, 'score': 1, 'reason': 'x'*9000}\n",
		"invalid-cost": "def score(request):\n    return {'pass': True, 'score': 1, 'cost': {'amount': -1, 'currency': 'USD', 'kind':'estimated', 'source':'fixture'}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			config := scorerConfig(t, code)
			result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
			if err == nil || result.Pass || result.ErrorCode != "malformed_result" {
				t.Fatalf("invalid result=%+v error=%v", result, err)
			}
			assertNoWorkers(t, config.OwnerID)
		})
	}
}

func TestPinnedJudgeKeepsRubricSeparateAndRecordsCost(t *testing.T) {
	config := scorerConfig(t, `def score(request):
    assert request["rubric"] == "Reward exactly the approved output."
    assert request["parameters"] == {"temperature": "0", "seed": "7"}
    assert request["judge_model_digest"] == "sha256:" + "a"*64
    return {"pass": request["output"] == "approved", "score": 0.75, "reason": "synthetic judge", "cost": {"amount": 0.002, "currency": "USD", "kind": "estimated", "source": "synthetic-price-v1"}}
`)
	rubric := []byte("Reward exactly the approved output.")
	config.RubricPath = filepath.Join(t.TempDir(), "rubric.txt")
	if err := os.WriteFile(config.RubricPath, rubric, 0644); err != nil {
		t.Fatal(err)
	}
	config.RubricDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(rubric))
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{Output: "approved", ScorerDigest: config.CodeDigest, RubricDigest: config.RubricDigest, JudgeModelDigest: "sha256:" + strings.Repeat("a", 64), Parameters: map[string]string{"temperature": "0", "seed": "7"}})
	if err != nil || !result.Pass || result.Score != 0.75 || result.Cost == nil || result.Cost.Amount != 0.002 || result.Cost.Kind != "estimated" || result.Cost.Source != "synthetic-price-v1" {
		t.Fatalf("judge=%+v error=%v", result, err)
	}
	assertNoWorkers(t, config.OwnerID)
}

func TestChangedScorerArtifactCannotExecute(t *testing.T) {
	config := scorerConfig(t, "def score(request):\n    return {'pass': True, 'score': 1}\n")
	if err := os.WriteFile(config.CodePath, []byte("raise RuntimeError('tampered')"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
	if err == nil || result.Pass || result.ErrorCode != "artifact_error" {
		t.Fatalf("tampered result=%+v error=%v", result, err)
	}
	assertNoWorkers(t, config.OwnerID)
}

func assertNoWorkers(t *testing.T, owner string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet", "--filter", "label=hypercall.scorer.owner="+owner).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("worker cleanup failed: %s, %v", out, err)
	}
}

func TestScorerPIDAndTemporaryStorageLimitsAreEnforced(t *testing.T) {
	config := scorerConfig(t, `import errno, os, subprocess

def score(request):
    children = []
    denied = False
    try:
        for _ in range(80):
            try:
                child = subprocess.Popen(["sleep", "30"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            except OSError as error:
                assert error.errno == errno.EAGAIN
                denied = True
                break
            children.append(child)
        assert denied and len(children) < 64, "PID limit did not stop forks"
    finally:
        for child in children:
            child.kill()
            child.wait()
    try:
        with open("/tmp/fill", "wb") as fill:
            for _ in range(20):
                fill.write(b'x' * 1024 * 1024)
        raise AssertionError("temporary storage unbounded")
    except OSError as error:
        assert error.errno == errno.ENOSPC
    finally:
        os.unlink("/tmp/fill")
    return {"pass": True, "score": 1, "reason": "limits enforced"}
`)
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
	if err != nil || !result.Pass {
		t.Fatalf("limits=%+v error=%v", result, err)
	}
	assertNoWorkers(t, config.OwnerID)
}

func TestScorerMemoryExhaustionCannotPass(t *testing.T) {
	config := scorerConfig(t, "def score(request):\n    allocation = bytearray(256*1024*1024)\n    return {'pass':True, 'score':1}\n")
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
	if err == nil || result.Pass || result.ErrorCode != "worker_error" {
		t.Fatalf("memory=%+v error=%v", result, err)
	}
	assertNoWorkers(t, config.OwnerID)
}

func TestCancelledScorerCannotPassOrLeaveWorker(t *testing.T) {
	config := scorerConfig(t, "import time\ndef score(request):\n    time.sleep(300)\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(time.Second, cancel)
	defer timer.Stop()
	result, err := scorer.NewRunner(config).Evaluate(ctx, scorer.Request{ScorerDigest: config.CodeDigest})
	if err == nil || result.Pass || result.ErrorCode != "cancelled" {
		t.Fatalf("cancelled=%+v error=%v", result, err)
	}
	assertNoWorkers(t, config.OwnerID)
}

func TestRecoveryDeletesOnlyFencedOwnerWorkers(t *testing.T) {
	config := scorerConfig(t, "def score(request):\n    return {'pass':False, 'score':0}\n")
	newerOwner := config.OwnerID + "-new"
	for _, owner := range []string{config.OwnerID, newerOwner} {
		out, err := exec.Command("docker", "create", "--label", "hypercall.scorer.owner="+owner, config.ImageDigest).CombinedOutput()
		if err != nil {
			t.Fatalf("prepare orphan: %s %v", out, err)
		}
		id := strings.TrimSpace(string(out))
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", id).Run() })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := scorer.NewRunner(config).Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoWorkers(t, config.OwnerID)
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet", "--filter", "label=hypercall.scorer.owner="+newerOwner).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("recovery removed newer owner: %s %v", out, err)
	}
}

func TestScorerArtifactSymlinksCannotExecute(t *testing.T) {
	config := scorerConfig(t, "def score(request):\n    return {'pass': True, 'score': 1}\n")
	target := config.CodePath + ".target"
	if err := os.Rename(config.CodePath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, config.CodePath); err != nil {
		t.Fatal(err)
	}
	result, err := scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
	if err == nil || result.Pass || result.ErrorCode != "artifact_error" {
		t.Fatalf("symlink result=%+v error=%v", result, err)
	}
}

// Helper is a separate evaluator process: killing it bypasses every Go defer.
func TestScorerEvaluatorCrashHelper(t *testing.T) {
	raw := os.Getenv("HYPERCALL_SCORER_CRASH_CONFIG")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var config scorer.Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	_, _ = scorer.NewRunner(config).Evaluate(context.Background(), scorer.Request{ScorerDigest: config.CodeDigest})
}

func TestScorerStopsWhenEvaluatorIsKilled(t *testing.T) {
	config := scorerConfig(t, "import os, signal, time\ndef score(request):\n    os.kill(1, signal.SIGSTOP)\n    try:\n        open('/proc/1/mem','rb+')\n        raise RuntimeError('watchdog memory writable')\n    except PermissionError:\n        pass\n    open('/tmp/scorer-started','w').close()\n    while True: time.sleep(1)\n")
	config.Timeout = 6 * time.Second
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestScorerEvaluatorCrashHelper$")
	child.Env = append(os.Environ(), "HYPERCALL_SCORER_CRASH_CONFIG="+string(encoded))
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.Process != nil {
			_ = child.Process.Kill()
		}
	})
	t.Cleanup(func() { _ = scorer.NewRunner(config).Recover(context.Background()) })
	var id string
	ready := false
	start := time.Now()
	for time.Since(start) < 4*time.Second {
		out, _ := exec.Command("docker", "ps", "--quiet", "--no-trunc", "--filter", "label=hypercall.scorer.owner="+config.OwnerID).Output()
		id = strings.TrimSpace(string(out))
		if id != "" && exec.Command("docker", "exec", id, "test", "-f", "/tmp/scorer-started").Run() == nil {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal("scorer never started")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	// No parent-side cancellation or Recover is allowed until observing stop.
	stopped := false
	for time.Since(start) < 9*time.Second {
		out, inspectErr := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", id).Output()
		if inspectErr == nil && strings.TrimSpace(string(out)) == "false" {
			stopped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("scorer outlived its evaluator and its independent lifetime")
	}
	if err = scorer.NewRunner(config).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoWorkers(t, config.OwnerID)
}

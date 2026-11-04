// Package scorer executes pinned Python artifacts inside bounded OCI workers.
package scorer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	scorerv1 "github.com/gduchidze/hypercall/gen/go/hypercall/scorer/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

const maxMessage = 1024 * 1024

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var ownerPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var containerPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Request pins both executable artifacts and judge configuration. Variables and
// output are data; no expression expansion or host Python execution occurs.
type Request struct {
	Output                                       string
	Variables                                    map[string]string
	ScorerDigest, RubricDigest, JudgeModelDigest string
	Parameters                                   map[string]string
}
type Cost struct {
	Amount                 float64
	Currency, Kind, Source string
}
type Result struct {
	Pass              bool
	Score             float64
	Reason, ErrorCode string
	Cost              *Cost
}
type Config struct {
	ImageDigest, CodeDigest, RubricDigest, CodePath, RubricPath, OwnerID, DockerBinary string
	CPU                                                                                float64
	MemoryMB, PIDs                                                                     int
	Timeout                                                                            time.Duration
}
type Runner struct{ config Config }

func NewRunner(config Config) *Runner {
	if config.DockerBinary == "" {
		config.DockerBinary = "docker"
	}
	return &Runner{config: config}
}
func failure(code string, err error) (Result, error) {
	reason := ""
	if err != nil {
		reason = err.Error()
		if len(reason) > 8192 {
			reason = reason[:8192]
		}
	}
	return Result{ErrorCode: code, Reason: reason}, err
}

func (r *Runner) validate(request Request) error {
	c := r.config
	if !digestPattern.MatchString(c.ImageDigest) || !digestPattern.MatchString(c.CodeDigest) || !ownerPattern.MatchString(c.OwnerID) {
		return errors.New("scorer requires pinned image, code and owner identity")
	}
	if c.CPU <= 0 || c.CPU > 64 || math.IsNaN(c.CPU) || math.IsInf(c.CPU, 0) || c.MemoryMB < 64 || c.MemoryMB > 65536 || c.PIDs < 32 || c.PIDs > 4096 || c.Timeout <= 0 || c.Timeout > time.Hour {
		return errors.New("scorer resource limits outside supported range")
	}
	if request.ScorerDigest != c.CodeDigest || request.RubricDigest != c.RubricDigest {
		return errors.New("request artifact pins do not match worker")
	}
	if c.RubricDigest != "" && !digestPattern.MatchString(c.RubricDigest) {
		return errors.New("invalid rubric digest")
	}
	if request.JudgeModelDigest != "" && (!digestPattern.MatchString(request.JudgeModelDigest) || c.RubricDigest == "" || len(request.Parameters) == 0) {
		return errors.New("judge requires pinned model, rubric and parameters")
	}
	return nil
}

func requestProto(request Request) *scorerv1.ScoreRequest {
	return &scorerv1.ScoreRequest{Output: request.Output, Variables: request.Variables, ScorerDigest: request.ScorerDigest, RubricDigest: request.RubricDigest, JudgeModelDigest: request.JudgeModelDigest, Parameters: request.Parameters}
}

// stageArtifact copies verified bytes into invocation-private immutable mounts.
// The worker verifies the copy again before importing any scorer code.
func stageArtifact(source, digest, destination string) error {
	descriptor, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(descriptor), source)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMessage {
		return errors.New("artifact must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMessage+1))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if len(data) > maxMessage || "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return errors.New("artifact digest mismatch")
	}
	return os.WriteFile(destination, data, 0444)
}

// Evaluate performs a single RPC in a fresh restricted container. Its deadline
// includes image verification, startup, scoring and transport establishment.
func (r *Runner) Evaluate(parent context.Context, request Request) (result Result, returned error) {
	if err := r.validate(request); err != nil {
		return failure("invalid_configuration", err)
	}
	wire := requestProto(request)
	if proto.Size(wire) > maxMessage {
		return failure("request_too_large", errors.New("scorer request exceeds 1 MiB"))
	}
	ctx, cancel := context.WithTimeout(parent, r.config.Timeout)
	defer cancel()
	directory, err := os.MkdirTemp("", stagePrefix(r.config.OwnerID))
	if err != nil {
		return failure("worker_error", err)
	}
	defer os.RemoveAll(directory)
	// Docker daemon must share the local filesystem; remote daemons fail closed.
	codePath := filepath.Join(directory, "scorer.py")
	if err = stageArtifact(r.config.CodePath, r.config.CodeDigest, codePath); err != nil {
		return failure("artifact_error", err)
	}
	rubricPath := filepath.Join(directory, "rubric.txt")
	if r.config.RubricDigest != "" {
		if err = stageArtifact(r.config.RubricPath, r.config.RubricDigest, rubricPath); err != nil {
			return failure("artifact_error", err)
		}
	}
	actual, err := r.command(ctx, "image", "inspect", "--format", "{{.Id}}", r.config.ImageDigest)
	if err != nil {
		if ctx.Err() != nil {
			return r.executionFailure(ctx, err)
		}
		return failure("image_error", fmt.Errorf("pinned scorer image unavailable: %w", err))
	}
	if strings.TrimSpace(actual) != r.config.ImageDigest {
		return failure("image_error", errors.New("pinned scorer image identity mismatch"))
	}
	nonceBytes := make([]byte, 32)
	if _, err = rand.Read(nonceBytes); err != nil {
		return failure("worker_error", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	name := "hypercall-scorer-" + nonce[:24]
	// Named ownership labels permit fenced restart recovery without global prune.
	args := []string{"create", "--name", name, "--label", "hypercall.scorer.owner=" + r.config.OwnerID, "--label", "hypercall.scorer.invocation=" + nonce,
		"--log-driver", "none", "--network", "none", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--memory", fmt.Sprintf("%dm", r.config.MemoryMB), "--memory-swap", fmt.Sprintf("%dm", r.config.MemoryMB), "--cpus", fmt.Sprint(r.config.CPU), "--pids-limit", fmt.Sprint(r.config.PIDs),
		"--ulimit", "nofile=64:64", "--restart", "no", "--stop-timeout", "1", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216,mode=1777",
		"--env", "HYPERCALL_SCORER_LIFETIME_SECONDS=" + fmt.Sprintf("%.9f", r.config.Timeout.Seconds()), "--env", "HYPERCALL_SCORER_NONCE=" + nonce, "--env", "HYPERCALL_SCORER_DIGEST=" + r.config.CodeDigest, "--env", "HYPERCALL_RUBRIC_DIGEST=" + r.config.RubricDigest,
		"--mount", "type=bind,src=" + codePath + ",dst=/artifact/scorer.py,readonly"}
	if r.config.RubricDigest != "" {
		args = append(args, "--mount", "type=bind,src="+rubricPath+",dst=/artifact/rubric.txt,readonly")
	}
	args = append(args, r.config.ImageDigest)
	// Cleanup by our unpredictable name even if create succeeds after its client
	// loses the response. A bounded independent context survives caller timeout.
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		_, cleanErr := r.command(cleanCtx, "rm", "--force", name)
		if cleanErr != nil {
			returned = errors.Join(returned, fmt.Errorf("scorer cleanup: %w", cleanErr))
			result.Pass = false
			result.Reason += "; scorer cleanup: " + cleanErr.Error()
			if len(result.Reason) > 8192 {
				result.Reason = result.Reason[:8192]
			}
			if result.ErrorCode == "" {
				result.ErrorCode = "cleanup_error"
			}
		}
	}()
	id, err := r.command(ctx, args...)
	if err != nil {
		return r.executionFailure(ctx, err)
	}
	id = strings.TrimSpace(id)
	if !containerPattern.MatchString(id) {
		return failure("worker_error", errors.New("Docker returned invalid container identity"))
	}
	if _, err = r.command(ctx, "start", id); err != nil {
		return r.executionFailure(ctx, err)
	}
	conn, err := grpc.NewClient("passthrough:///isolated-scorer", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return r.relay(ctx, id) }), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage), grpc.MaxCallSendMsgSize(maxMessage)))
	if err != nil {
		return r.executionFailure(ctx, err)
	}
	defer conn.Close()
	reply, err := scorerv1.NewScorerClient(conn).Score(metadata.AppendToOutgoingContext(ctx, "x-hypercall-nonce", nonce), wire, grpc.WaitForReady(true))
	if err != nil {
		return r.executionFailure(ctx, err)
	}
	if reply.ErrorCode != "" {
		return failure(reply.ErrorCode, fmt.Errorf("isolated scorer failed: %s", reply.Reason))
	}
	if math.IsNaN(reply.Score) || math.IsInf(reply.Score, 0) || reply.Score < 0 || reply.Score > 1 || len(reply.Reason) > 8192 {
		return failure("malformed_result", errors.New("invalid scorer result"))
	}
	result = Result{Pass: reply.Pass, Score: reply.Score, Reason: reply.Reason}
	if cost := reply.Cost; cost != nil {
		if math.IsNaN(cost.Amount) || math.IsInf(cost.Amount, 0) || cost.Amount < 0 || (cost.Kind != "measured" && cost.Kind != "estimated" && cost.Kind != "synthetic") || len(cost.Currency) != 3 || cost.Source == "" || len(cost.Source) > 1024 {
			return failure("malformed_result", errors.New("invalid scorer cost"))
		}
		result.Cost = &Cost{Amount: cost.Amount, Currency: cost.Currency, Kind: cost.Kind, Source: cost.Source}
	}
	return result, nil
}
func (r *Runner) executionFailure(ctx context.Context, err error) (Result, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failure("deadline_exceeded", errors.Join(ctx.Err(), err))
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return failure("cancelled", errors.Join(ctx.Err(), err))
	}
	return failure("worker_error", err)
}

// Recover removes containers of a previous fenced evaluator owner. The caller
// must establish exclusive ownership first and use a new OwnerID per lease
// epoch; it must never recover its currently active epoch.
func (r *Runner) Recover(ctx context.Context) error {
	if !ownerPattern.MatchString(r.config.OwnerID) {
		return errors.New("invalid scorer owner")
	}
	out, err := r.command(ctx, "ps", "--all", "--quiet", "--no-trunc", "--filter", "label=hypercall.scorer.owner="+r.config.OwnerID)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(out) {
		if !containerPattern.MatchString(id) {
			return errors.New("invalid recovery container identity")
		}
		if _, err = r.command(ctx, "rm", "--force", id); err != nil {
			return err
		}
	}
	// Invocation directories are scoped by a hash of the fenced lease owner.
	// Distinct owner epochs cannot share this prefix, including textual prefixes.
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stagePrefix(r.config.OwnerID)) {
			if err = os.RemoveAll(filepath.Join(os.TempDir(), entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func stagePrefix(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return "hypercall-scorer-" + hex.EncodeToString(sum[:12]) + "-"
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 16384 {
		keep := 16384 - b.Len()
		if keep > len(p) {
			keep = len(p)
		}
		_, _ = b.Buffer.Write(p[:keep])
	}
	return n, nil
}
func (r *Runner) command(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.config.DockerBinary, args...)
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

type relayConn struct {
	reader io.ReadCloser
	writer io.WriteCloser
	cmd    *exec.Cmd
	once   sync.Once
}

func (r *Runner) relay(ctx context.Context, id string) (net.Conn, error) {
	cmd := exec.CommandContext(ctx, r.config.DockerBinary, "exec", "--interactive", id, "python", "-m", "hypercall_scorer.relay")
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	writer, err := cmd.StdinPipe()
	if err != nil {
		reader.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	conn := &relayConn{reader: reader, writer: writer, cmd: cmd}
	go func() { _ = cmd.Wait() }()
	return conn, nil
}
func (c *relayConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *relayConn) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *relayConn) Close() error {
	c.once.Do(func() {
		_ = c.writer.Close()
		_ = c.reader.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
	return nil
}
func (c *relayConn) LocalAddr() net.Addr              { return unixAddr("evaluator") }
func (c *relayConn) RemoteAddr() net.Addr             { return unixAddr("/tmp/scorer.sock") }
func (c *relayConn) SetDeadline(time.Time) error      { return nil }
func (c *relayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *relayConn) SetWriteDeadline(time.Time) error { return nil }

type unixAddr string

func (a unixAddr) Network() string { return "unix" }
func (a unixAddr) String() string  { return string(a) }

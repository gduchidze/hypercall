package guest

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This is deliberately one controlled qualification workload. Arbitrary programs
// with cached userspace RNGs are not supported by the explicit clone RNG policy.
const controlledPython = `import os,sys,json,random
counter=int(sys.argv[1])
with open('controlled-origin.txt','wb') as origin:
    origin.write(('counter='+str(counter)+'\n').encode()); origin.flush(); os.fsync(origin.fileno())
random.seed(os.urandom(32))
for line in sys.stdin:
    req=json.loads(line)
    if 'seed' in req: random.seed(bytes.fromhex(req['seed']))
    counter+=req.get('delta',0)
    record={'counter':counter,'identity':req['identity'],'random_probe':random.randbytes(32).hex()}
    data=json.dumps(record,sort_keys=True).encode()
    with open('controlled-state.json','wb') as f:
        f.write(data); f.flush(); os.fsync(f.fileno())
    os.sync()
    print(json.dumps({'counter':counter,'file_data':data.hex(),'pid':os.getpid(),'random_probe':record['random_probe']}),flush=True)
`

type controlledProcess struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	cleanup func() error
	state   *guestv1.ControlledState
}

func (s *Server) ordinary() error {
	if s.captureGate {
		return status.Error(codes.FailedPrecondition, "capture barrier requires session renewal before workload access")
	}
	return nil
}
func (s *Server) StartControlled(ctx context.Context, req *guestv1.StartControlledRequest) (*guestv1.ControlledState, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if err := s.ordinary(); err != nil {
		return nil, err
	}
	if s.controlled != nil {
		return nil, status.Error(codes.AlreadyExists, "controlled workload already exists")
	}
	if len(s.execSlots) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "foreground workloads must complete")
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, fileError(err)
	}
	defer dir.Close()
	cmd := exec.Command(s.execHelper, "--hypercall-internal-exec", "/usr/bin/python3", "-I", "-u", "-c", controlledPython, strconv.FormatInt(req.Counter, 10))
	cmd.ExtraFiles = []*os.File{dir}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !s.trustedLocal {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid), Groups: []uint32{uint32(s.gid)}}
	}
	group, err := commandCgroup(s.workloadCgroup, cmd)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "workload containment unavailable")
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		group.close()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		group.close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		group.close()
		return nil, status.Error(codes.FailedPrecondition, "controlled Python unavailable")
	}
	cp := &controlledProcess{cmd: cmd, input: input, output: bufio.NewReaderSize(output, 8192), cleanup: func() error {
		_ = group.kill()
		_ = killGroup(cmd)
		_ = input.Close()
		_ = cmd.Wait()
		return group.close()
	}}
	s.controlled = cp
	state, err := s.controlledCommand(0, nil)
	if err != nil {
		_ = cp.cleanup()
		s.controlled = nil
		return nil, err
	}
	return state, nil
}
func (s *Server) controlledCommand(delta int64, seed []byte) (*guestv1.ControlledState, error) {
	cp := s.controlled
	if cp == nil {
		return nil, status.Error(codes.FailedPrecondition, "controlled workload not started")
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	req := map[string]any{"delta": delta, "identity": s.identity}
	if seed != nil {
		req["seed"] = hex.EncodeToString(seed)
	}
	data, _ := json.Marshal(req)
	if _, err := cp.input.Write(append(data, '\n')); err != nil {
		return nil, status.Error(codes.Unavailable, "controlled process input failed")
	}
	line, err := cp.output.ReadSlice('\n')
	if err != nil {
		return nil, status.Error(codes.Unavailable, "controlled process barrier failed")
	}
	var result struct {
		Counter     int64  `json:"counter"`
		FileData    string `json:"file_data"`
		PID         int64  `json:"pid"`
		RandomProbe string `json:"random_probe"`
	}
	if json.Unmarshal(line, &result) != nil {
		return nil, status.Error(codes.Internal, "invalid controlled barrier")
	}
	file, err := hex.DecodeString(result.FileData)
	if err != nil {
		return nil, err
	}
	probe, err := hex.DecodeString(result.RandomProbe)
	if err != nil || len(probe) != 32 {
		return nil, errors.New("invalid RNG probe")
	}
	cp.state = &guestv1.ControlledState{Counter: result.Counter, FileData: file, Pid: result.PID, RandomProbe: probe, AtBarrier: true, Identity: proto.Clone(s.identity).(*guestv1.AllocationIdentity)}
	return proto.Clone(cp.state).(*guestv1.ControlledState), nil
}
func (s *Server) InspectControlled(ctx context.Context, _ *guestv1.InspectControlledRequest) (*guestv1.ControlledState, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if s.captureGate {
		return nil, status.Error(codes.Unauthenticated, "session renewal required at capture gate")
	}
	if s.controlled == nil {
		return nil, status.Error(codes.NotFound, "controlled workload not started")
	}
	s.controlled.mu.Lock()
	defer s.controlled.mu.Unlock()
	return proto.Clone(s.controlled.state).(*guestv1.ControlledState), nil
}
func (s *Server) ContinueControlled(ctx context.Context, req *guestv1.ContinueControlledRequest) (*guestv1.ControlledState, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if err := s.ordinary(); err != nil {
		return nil, err
	}
	if req.Delta < -1000000 || req.Delta > 1000000 {
		return nil, status.Error(codes.InvalidArgument, "delta exceeds controlled envelope")
	}
	return s.controlledCommand(req.Delta, nil)
}
func (s *Server) PrepareCapture(ctx context.Context, _ *guestv1.PrepareCaptureRequest) (*guestv1.ControlledState, error) {
	if !s.opMu.TryLock() {
		return nil, status.Error(codes.FailedPrecondition, "guest operations in flight")
	}
	defer s.opMu.Unlock()
	if s.captureGate {
		return nil, status.Error(codes.FailedPrecondition, "capture gate already closed")
	}
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if s.harness != nil {
		marker, e := s.harness.semanticBarrier()
		if e != nil {
			return nil, e
		}
		s.captureGate = true
		syncGuestDisks()
		return &guestv1.ControlledState{AtBarrier: true, Identity: proto.Clone(s.identity).(*guestv1.AllocationIdentity), SemanticBarrier: marker}, nil
	}
	if s.controlled == nil || s.controlled.state == nil || !s.controlled.state.AtBarrier {
		return nil, status.Error(codes.FailedPrecondition, "controlled Python barrier required")
	}
	s.captureGate = true
	syncGuestDisks()
	return proto.Clone(s.controlled.state).(*guestv1.ControlledState), nil
}
func (s *Server) ReleaseCapture(ctx context.Context, req *guestv1.ReleaseCaptureRequest) (*guestv1.ControlledState, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if !s.captureGate {
		return nil, status.Error(codes.FailedPrecondition, "parent is not capture-gated")
	}
	// Parent retains its bootstrap identity. A clone must use RebindClone instead.
	session, err := readCloneSession()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "session disk verification failed")
	}
	if !proto.Equal(session.Identity, s.identity) || session.CredentialHash() != s.credentialHash {
		return nil, status.Error(codes.Unauthenticated, "parent session mismatch")
	}
	if err = refreshGuestClock(req.HostUnixNano, session.ExpiresAt); err != nil {
		return nil, err
	}
	s.captureGate = false
	if s.harness != nil {
		s.harness.mu.Lock()
		marker := proto.Clone(s.harness.captureMarker).(*harnessv1.SemanticBarrier)
		s.harness.mu.Unlock()
		return &guestv1.ControlledState{AtBarrier: true, Identity: proto.Clone(s.identity).(*guestv1.AllocationIdentity), SemanticBarrier: marker}, nil
	}
	if s.controlled == nil {
		return nil, status.Error(codes.FailedPrecondition, "controlled workload missing")
	}
	return proto.Clone(s.controlled.state).(*guestv1.ControlledState), nil
}
func (s *Server) RebindClone(ctx context.Context, req *guestv1.RebindCloneRequest) (*guestv1.GetInfoResponse, error) {
	if s.harness != nil {
		return s.rebindAgentClone(ctx, req)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !s.captureGate {
		return nil, status.Error(codes.FailedPrecondition, "clone is not capture-gated")
	}
	session, err := readCloneSession()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "fresh clone session disk unavailable")
	}
	if err = session.ValidateClone(s.identity, ctx); err != nil {
		return nil, err
	}
	if session.CredentialHash() == s.credentialHash {
		return nil, status.Error(codes.FailedPrecondition, "clone credential must differ from captured session")
	}
	if err = refreshGuestClock(req.HostUnixNano, session.ExpiresAt); err != nil {
		return nil, err
	}
	entropy, err := hex.DecodeString(session.Entropy)
	if err != nil || len(entropy) != 32 {
		return nil, status.Error(codes.FailedPrecondition, "fresh 256-bit entropy required")
	}
	if err = reseedKernel(entropy); err != nil {
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("kernel RNG renewal failed: %v", err))
	}
	// Do not reopen workload RPCs until the controlled process has reseeded too.
	s.identity = proto.Clone(session.Identity).(*guestv1.AllocationIdentity)
	s.credentialHash = session.CredentialHash()
	s.expiresAt = session.ExpiresAt
	if _, err = s.controlledCommand(0, entropy); err != nil {
		return nil, err
	}
	s.captureGate = false
	return &guestv1.GetInfoResponse{ProtocolVersion: "hypercall.guest.v1", Identity: proto.Clone(s.identity).(*guestv1.AllocationIdentity), MaxFileBytes: MaxFileBytes, MaxExecOutputBytes: MaxExecOutputBytes, ServerTimeUnixNano: time.Now().UnixNano()}, nil
}

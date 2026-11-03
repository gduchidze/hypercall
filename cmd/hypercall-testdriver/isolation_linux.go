//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/sandbox"
)

const resourceInfoScript = `import json,os
p='/sys/fs/cgroup'+open('/proc/self/cgroup').read().strip().split(':')[-1]
def finite(name):
 q=p
 while q.startswith('/sys/fs/cgroup'):
  try:
   v=open(q+'/'+name).read().strip()
   if v!='max' and not v.startswith('max '): return v
  except OSError: pass
  q=os.path.dirname(q)
 raise RuntimeError('missing finite '+name)
def events():
 q=p
 while q.startswith('/sys/fs/cgroup'):
  try:
   v=open(q+'/memory.max').read().strip()
   if v!='max':return open(q+'/memory.events').read()
  except OSError:pass
  q=os.path.dirname(q)
 raise RuntimeError('missing memory events')
v=os.statvfs('.')
print(json.dumps({'memory_max':finite('memory.max'),'pids_max':finite('pids.max'),'cpu_max':finite('cpu.max'),'memory_events':events(),'workspace_bytes':v.f_blocks*v.f_frsize}))
`

func resourceInfo(s *suite, a sandbox.Observation) (map[string]any, error) {
	r, err := s.pythonExec(a, resourceInfoScript, 3000)
	if err != nil {
		return nil, err
	}
	if r.result == nil || r.result.ExitCode != 0 {
		return nil, fmt.Errorf("resource introspection failed: %s stderr=%s", r.summary(), r.stderr)
	}
	var m map[string]any
	if err := json.Unmarshal(r.stdout, &m); err != nil {
		return nil, err
	}
	return m, nil
}
func oomCount(m map[string]any) int64 {
	lines := strings.Fields(fmt.Sprint(m["memory_events"]))
	for i := 0; i+1 < len(lines); i += 2 {
		if lines[i] == "oom_kill" {
			v, _ := strconv.ParseInt(lines[i+1], 10, 64)
			return v
		}
	}
	return -1
}
func (s *suite) limits(a sandbox.Observation) (map[string]any, error) {
	before, err := resourceInfo(s, a)
	if err != nil {
		return nil, err
	}
	if before["memory_max"] != "201326592" || before["pids_max"] != "32" || before["cpu_max"] != "100000 100000" || before["workspace_bytes"] != float64(8<<20) {
		return before, errors.New("guest resource envelope differs from admitted 192MiB workload/32PIDs/1CPU/8MiB workspace")
	}
	pids, err := s.pythonExec(a, `import errno,json,os,signal,time
children=[]
failed=False
try:
 for n in range(64):
  try: pid=os.fork()
  except OSError as e:
   if e.errno!=errno.EAGAIN:raise
   failed=True
   break
  if pid==0:
   time.sleep(20)
   os._exit(0)
  children.append(pid)
finally:
 for pid in children:
  os.kill(pid,signal.SIGKILL)
 for pid in children:
  os.waitpid(pid,0)
print(json.dumps({'fork_limit_enforced':failed,'children':len(children)}))
assert failed and 0<len(children)<32
`, 5000)
	if err != nil || pids.result == nil || pids.result.ExitCode != 0 {
		return nil, fmt.Errorf("PID exhaustion not enforced/recovered: %v %s stderr=%s", err, pids.summary(), pids.stderr)
	}
	disk, err := s.pythonExec(a, `import errno,json,os
full=False
try:
 with open('fill-workspace','wb',buffering=0) as f:
  for n in range(256):f.write(b'x'*65536)
except OSError as e:
 if e.errno!=errno.ENOSPC:raise
 full=True
finally:
 try:os.unlink('fill-workspace')
 except FileNotFoundError:pass
assert full
try:
 open('/run/hypercall/session.json','rb').read()
except PermissionError:pass
else:raise AssertionError('root session secret readable')
try:
 open('/etc/qualification-escape','wb').write(b'x')
except OSError as e:
 assert e.errno in (errno.EROFS,errno.EACCES)
else:raise AssertionError('rootfs writable')
print(json.dumps({'workspace_enospc':full,'rootfs_write_denied':True,'session_secret_read_denied':True}))
`, 5000)
	if err != nil || disk.result == nil || disk.result.ExitCode != 0 {
		return nil, fmt.Errorf("workspace bound failed: %v %s stderr=%s", err, disk.summary(), disk.stderr)
	}
	mem, err := s.pythonExec(a, `blocks=[]
for n in range(512):blocks.append(bytearray(1024*1024))
raise AssertionError('memory cap not enforced')
`, 10000)
	if err != nil {
		return nil, fmt.Errorf("memory stress RPC failed rather than isolating workload: %w", err)
	}
	if mem.result == nil || mem.result.ExitCode == 0 || mem.result.TimedOut {
		return nil, fmt.Errorf("memory stress not killed: %s", mem.summary())
	}
	after, err := resourceInfo(s, a)
	if err != nil {
		return nil, err
	}
	if oomCount(after) <= oomCount(before) {
		return nil, errors.New("nonzero memory stress exit lacked memory.events oom_kill increment")
	}
	admittedCPU, err := os.ReadFile(a.CgroupPath + "/cpu.max")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(admittedCPU)) != "100000 100000" {
		return nil, errors.New("host CPU admission cap mismatch")
	}
	if err = os.WriteFile(a.CgroupPath+"/cpu.max", []byte("50000 100000"), 0600); err != nil {
		return nil, err
	}
	defer func() { _ = os.WriteFile(a.CgroupPath+"/cpu.max", admittedCPU, 0600) }()
	cpuBeforeBytes, err := os.ReadFile(a.CgroupPath + "/cpu.stat")
	if err != nil {
		return nil, err
	}
	cpuStart := time.Now()
	cpu, err := s.pythonExec(a, "import time; end=time.process_time()+2\nwhile time.process_time()<end: pass", 10000)
	cpuWall := time.Since(cpuStart)
	if err != nil || cpu.result == nil || cpu.result.ExitCode != 0 {
		return nil, fmt.Errorf("CPU stress failed: %v %s", err, cpu.summary())
	}
	cpuAfterBytes, err := os.ReadFile(a.CgroupPath + "/cpu.stat")
	if err != nil {
		return nil, err
	}
	cpuBefore, cpuAfter := parseCounters(string(cpuBeforeBytes)), parseCounters(string(cpuAfterBytes))
	usage := cpuAfter["usage_usec"] - cpuBefore["usage_usec"]
	throttled := cpuAfter["nr_throttled"] - cpuBefore["nr_throttled"]
	if usage < 1500000 || usage > cpuWall.Microseconds()*65/100 || throttled <= 0 {
		return nil, fmt.Errorf("CPU quota stress lacks bounded usage/throttling evidence: usage_usec=%d wall_usec=%d nr_throttled=%d", usage, cpuWall.Microseconds(), throttled)
	}
	if err = os.WriteFile(a.CgroupPath+"/cpu.max", admittedCPU, 0600); err != nil {
		return nil, err
	}
	restoredCPU, err := os.ReadFile(a.CgroupPath + "/cpu.max")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(restoredCPU)) != strings.TrimSpace(string(admittedCPU)) {
		return nil, errors.New("operator CPU probe quota was not restored")
	}
	host := map[string]any{"cpu_probe_operator_quota": "50000 100000", "admitted_cpu_quota": strings.TrimSpace(string(admittedCPU)), "admitted_cpu_quota_restored": true, "cpu_stress_before": cpuBefore, "cpu_stress_after": cpuAfter, "cpu_stress_wall_ms": cpuWall.Milliseconds(), "cpu_stress_usage_usec": usage, "cpu_throttled_periods": throttled}
	for _, name := range []string{"memory.max", "memory.current", "memory.peak", "memory.events", "cpu.max", "cpu.stat", "pids.max", "pids.current", "pids.peak"} {
		v, e := os.ReadFile(a.CgroupPath + "/" + name)
		if e != nil {
			return nil, e
		}
		host[name] = strings.TrimSpace(string(v))
	}
	return map[string]any{"guest_before": before, "guest_after": after, "pids": json.RawMessage(pids.stdout), "workspace": json.RawMessage(disk.stdout), "memory_exit_code": mem.result.ExitCode, "host_cgroup": host}, nil
}
func (s *suite) network(a sandbox.Observation) (map[string]any, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	targets := []map[string]any{{"name": "internet", "ip": "1.1.1.1", "port": 80}, {"name": "metadata", "ip": "169.254.169.254", "port": 80}}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		addrs, e := iface.Addrs()
		if e != nil {
			return nil, e
		}
		for _, addr := range addrs {
			ip, _, e := net.ParseCIDR(addr.String())
			if e == nil && ip.To4() != nil && !ip.IsLoopback() {
				targets = append(targets, map[string]any{"name": "host", "ip": ip.String(), "port": port})
			}
		}
	}
	if len(targets) == 2 {
		return nil, errors.New("no non-loopback host IPv4 address to probe")
	}
	encoded, _ := json.Marshal(targets)
	script := `import json,os,socket
interfaces=os.listdir('/sys/class/net')
assert interfaces==['lo'],interfaces
targets=json.loads('` + string(encoded) + `')
for t in targets:
 s=socket.socket()
 s.settimeout(1)
 try:s.connect((t['ip'],t['port']))
 except OSError as e:t['errno']=e.errno
 else:raise AssertionError('guest reached '+t['name'])
 finally:s.close()
vsock=[]
for port in (1024,18080):
 s=socket.socket(socket.AF_VSOCK,socket.SOCK_STREAM)
 s.settimeout(1)
 try:s.connect((2,port))
 except OSError as e:vsock.append({'cid':2,'port':port,'errno':e.errno})
 else:raise AssertionError('unsolicited host vsock listener reachable')
 finally:s.close()
print(json.dumps({'interfaces':interfaces,'denied_targets':targets,'denied_host_vsock':vsock}))
`
	r, err := s.pythonExec(a, script, 7000)
	if err != nil || r.result == nil || r.result.ExitCode != 0 {
		return nil, fmt.Errorf("deny-network probe failed: %v %s stderr=%s", err, r.summary(), r.stderr)
	}
	listener.SetDeadline(time.Now().Add(100 * time.Millisecond))
	c, err := listener.Accept()
	if err == nil {
		c.Close()
		return nil, errors.New("host sentinel accepted guest connection")
	}
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		return nil, err
	}
	var evidence map[string]any
	err = json.Unmarshal(r.stdout, &evidence)
	return evidence, err
}
func (s *suite) isolation(a sandbox.Observation) (map[string]any, error) {
	other, err := s.create(5 * time.Minute)
	if err != nil {
		return nil, err
	}
	marker := []byte("private-to-primary")
	if _, err = s.b.WriteFile(s.ctx, a.Allocation, &guestv1.WriteFileRequest{Path: "isolation", Data: marker}); err != nil {
		return nil, err
	}
	if _, err = s.b.ReadFile(s.ctx, other.Allocation, &guestv1.ReadFileRequest{Path: "isolation"}); err == nil {
		return nil, errors.New("second VM read primary workspace")
	}
	if _, err = s.b.WriteFile(s.ctx, other.Allocation, &guestv1.WriteFileRequest{Path: "isolation", Data: []byte("private-to-second")}); err != nil {
		return nil, err
	}
	r, err := s.b.ReadFile(s.ctx, a.Allocation, &guestv1.ReadFileRequest{Path: "isolation"})
	if err != nil || string(r.Data) != string(marker) {
		return nil, errors.New("second VM write changed primary workspace")
	}
	if a.PID == other.PID || a.CgroupPath == other.CgroupPath || a.VSockPath == other.VSockPath || a.RootDir == other.RootDir {
		return nil, errors.New("VM process/cgroup/vsock/disk identity reused")
	}
	return map[string]any{"simultaneously_running_vms": 2, "private_workspaces": true, "distinct_pid_cgroup_transport_root": true}, nil
}
func (s *suite) guestFailure() (map[string]any, error) {
	evidence := map[string]any{}
	for _, mode := range []string{"vmm-death", "unresponsive-guest"} {
		e, err := s.guestFailureCase(mode)
		if err != nil {
			return evidence, fmt.Errorf("%s: %w", mode, err)
		}
		evidence[mode] = e
	}
	return evidence, nil
}
func (s *suite) guestFailureCase(mode string) (map[string]any, error) {
	a, err := s.create(5 * time.Minute)
	if err != nil {
		return nil, err
	}
	events, err := os.ReadFile(a.CgroupPath + "/cgroup.events")
	if err != nil {
		return nil, err
	}
	if !strings.Contains(string(events), "populated 1") {
		return nil, errors.New("VM cgroup was not populated before fault")
	}
	frozen := false
	if mode == "vmm-death" {
		if err = os.WriteFile(a.CgroupPath+"/cgroup.kill", []byte("1"), 0600); err != nil {
			return nil, err
		}
	} else {
		if err = os.WriteFile(a.CgroupPath+"/cgroup.freeze", []byte("1"), 0600); err != nil {
			return nil, err
		}
		defer func() { _ = os.WriteFile(a.CgroupPath+"/cgroup.freeze", []byte("0"), 0600) }()
	}
	start := time.Now()
	deadline := start.Add(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if mode == "unresponsive-guest" {
			events, _ := os.ReadFile(a.CgroupPath + "/cgroup.events")
			if strings.Contains(string(events), "frozen 1") {
				frozen = true
			}
		}
		// Inspect and Stop both reconcile state. Observe external cleanup first so
		// this check cannot pass merely because a read or explicit Stop cleaned up.
		if cleanObservation(a) == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("monitor did not autonomously clean failed VM")
		}
		select {
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-ticker.C:
		}
	}
	if mode == "unresponsive-guest" && !frozen {
		return nil, errors.New("unresponsive VM was not confirmed frozen")
	}
	obs, err := s.b.Inspect(s.ctx, a.Allocation)
	if err != nil {
		return nil, err
	}
	if obs.State != sandbox.StateFailed {
		return nil, fmt.Errorf("monitor failure state=%s, expected failed", obs.State)
	}
	return map[string]any{"vmm_populated_before_fault": true, "operator_fault": mode, "frozen_confirmed": frozen, "automatic_cleanup_ms": time.Since(start).Milliseconds(), "state": obs.State, "no_inspect_or_stop_before_cleanup": true}, nil
}

func parseCounters(v string) map[string]int64 {
	out := map[string]int64{}
	f := strings.Fields(v)
	for i := 0; i+1 < len(f); i += 2 {
		out[f[i]], _ = strconv.ParseInt(f[i+1], 10, 64)
	}
	return out
}

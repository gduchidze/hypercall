// Package hostdoctor qualifies host prerequisites. A passing report is not a
// sandbox boot, isolation certification, or Phase 1A completion result.
package hostdoctor

import (
	"context"
	"runtime"
)

type Check struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Detail   string            `json:"detail"`
	Evidence map[string]string `json:"evidence,omitempty"`
}

type Report struct {
	SchemaVersion int      `json:"schema_version"`
	Scope         string   `json:"scope"`
	OS            string   `json:"os"`
	Architecture  string   `json:"architecture"`
	Ready         bool     `json:"ready"`
	Checks        []Check  `json:"checks"`
	Blockers      []string `json:"blockers"`
}

type Options struct {
	ConfigPath string
	CgroupDir  string
}

func Run(ctx context.Context, options Options) Report {
	r := Report{SchemaVersion: 1, Scope: "host-prerequisites", OS: runtime.GOOS, Architecture: runtime.GOARCH, Ready: true, Checks: []Check{}, Blockers: []string{}}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		r.add(blocked("host.platform", "requires Linux amd64 or arm64; on supported Apple Silicon use the pinned Lima development VM"))
	} else {
		r.add(passed("host.platform", "Linux architecture supported for host prerequisite qualification; artifacts must match this architecture"))
	}
	for _, check := range platformChecks(ctx, options) {
		r.add(check)
	}
	for _, check := range artifactChecks(ctx, options.ConfigPath) {
		r.add(check)
	}
	return r
}

func (r *Report) add(check Check) {
	r.Checks = append(r.Checks, check)
	if check.Status != "passed" {
		r.Ready = false
		r.Blockers = append(r.Blockers, check.ID)
	}
}

func passed(id, detail string) Check  { return Check{ID: id, Status: "passed", Detail: detail} }
func blocked(id, detail string) Check { return Check{ID: id, Status: "blocked", Detail: detail} }

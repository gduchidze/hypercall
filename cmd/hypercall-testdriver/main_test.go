package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQualificationRequiresEveryGate(t *testing.T) {
	report := Report{SchemaVersion: 1, Checks: []Check{{ID: "boot", Passed: true}}}
	if report.Passed() {
		t.Fatal("incomplete report passed Phase 1A")
	}
	for _, id := range requiredChecks {
		report.Checks = append(report.Checks, Check{ID: id, Passed: true})
	}
	if !report.Passed() {
		t.Fatal("all required checks should pass")
	}
	report.Checks = append(report.Checks, Check{ID: "late-cleanup", Passed: false})
	if report.Passed() {
		t.Fatal("failed cleanup did not fail gate")
	}
}

func TestReportRedactsKnownCredentials(t *testing.T) {
	r := Report{Checks: []Check{{ID: "auth", Error: "rejected super-private-credential-value"}}}
	encoded, err := marshalReport(r, []string{"super-private-credential-value"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "super-private-credential-value") {
		t.Fatal("credential leaked")
	}
	var valid Report
	if err := json.Unmarshal(encoded, &valid); err != nil {
		t.Fatal(err)
	}
}

func TestQualificationRefusesImplicitConfiguration(t *testing.T) {
	if err := (Options{}).validate(); err == nil {
		t.Fatal("missing artifact configuration accepted")
	}
}

func TestSnapshotReportsRequireTheirOwnCompleteGates(t *testing.T) {
	for _, profile := range []string{"phase1b-local", "phase1b-capture", "phase1b-restore"} {
		r := Report{Profile: profile}
		if r.Passed() {
			t.Fatalf("empty %s report passed", profile)
		}
		for _, id := range snapshotChecks(profile) {
			r.Checks = append(r.Checks, Check{ID: id, Passed: true})
		}
		if !r.Passed() {
			t.Fatalf("complete %s report failed", profile)
		}
		r.Checks = append(r.Checks, Check{ID: "late-failure"})
		if r.Passed() {
			t.Fatalf("%s ignored late failure", profile)
		}
	}
	if (Report{Profile: "unknown"}).Passed() {
		t.Fatal("unrecognized acceptance profile passed")
	}
}

func TestSnapshotLocalReportRequiresCaptureExpiry(t *testing.T) {
	r := Report{Profile: "phase1b-local"}
	for _, id := range snapshotChecks(r.Profile) {
		if id != "capture-expiry" {
			r.Checks = append(r.Checks, Check{ID: id, Passed: true})
		}
	}
	if r.Passed() {
		t.Fatal("snapshot report omitted capture expiry evidence")
	}
}

func TestSnapshotReportsRequireRestoredClock(t *testing.T) {
	for _, profile := range []string{"phase1b-local", "phase1b-restore"} {
		r := Report{Profile: profile}
		for _, id := range snapshotChecks(profile) {
			if id != "restored-clock" {
				r.Checks = append(r.Checks, Check{ID: id, Passed: true})
			}
		}
		if r.Passed() {
			t.Fatalf("%s omitted restored UTC evidence", profile)
		}
	}
}

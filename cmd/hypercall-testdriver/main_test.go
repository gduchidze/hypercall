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

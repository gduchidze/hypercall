//go:build !linux

package main

import (
	"context"
	"errors"
	"time"
)

func qualify(_ context.Context, _ Options) (Report, []string) {
	r := newReport()
	r.Checks = []Check{{ID: "host-prerequisites", Error: "real qualification requires Linux/KVM"}}
	r.FinishedAt = time.Now().UTC()
	return r, nil
}
func hold(_ context.Context, _ Options) error {
	return errors.New("real qualification requires Linux/KVM")
}

func qualifySnapshot(_ context.Context, _ Options, command string) (Report, []string) {
	r := newReport()
	r.Profile = "phase1b-local"
	r.Checks = []Check{{ID: "host-prerequisites", Error: "real snapshot qualification requires Linux/KVM"}}
	r.FinishedAt = time.Now().UTC()
	return r, nil
}
func snapshotHold(_ context.Context, _ Options) error {
	return errors.New("real snapshot qualification requires Linux/KVM")
}

func snapshotUploadHold(_ context.Context, _ Options) error {
	return errors.New("real snapshot qualification requires Linux/KVM")
}

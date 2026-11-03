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

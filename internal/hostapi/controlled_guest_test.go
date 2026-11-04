package hostapi_test

import (
	"context"
	"os"
	"testing"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/hostapi"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

// Only startup/barrier uses an actual local guest. Capture/restore deliberately
// remain unavailable here: process tests cannot provide VM continuity evidence.
type controlledGuestDriver struct{ guestRuntime }

func (d *controlledGuestDriver) StartControlled(ctx context.Context, _ sandbox.Allocation, counter int64) (*guestv1.ControlledState, error) {
	return d.client.StartControlled(d.authorized(ctx), &guestv1.StartControlledRequest{Counter: counter})
}
func (d *controlledGuestDriver) InspectControlled(ctx context.Context, _ sandbox.Allocation) (*guestv1.ControlledState, error) {
	return d.client.InspectControlled(d.authorized(ctx), &guestv1.InspectControlledRequest{})
}
func (d *controlledGuestDriver) ContinueControlled(ctx context.Context, _ sandbox.Allocation, delta int64) (*guestv1.ControlledState, error) {
	return d.client.ContinueControlled(d.authorized(ctx), &guestv1.ContinueControlledRequest{Delta: delta})
}
func (d *controlledGuestDriver) Capture(context.Context, sandbox.CaptureRequest) (snapshot.Manifest, error) {
	return snapshot.Manifest{}, sandbox.ErrUnavailable
}
func (d *controlledGuestDriver) Restore(context.Context, sandbox.RestoreRequest) (sandbox.Observation, error) {
	return sandbox.Observation{}, sandbox.ErrUnavailable
}
func (d *controlledGuestDriver) Fork(context.Context, sandbox.ForkRequest) ([]sandbox.ChildResult, error) {
	return nil, sandbox.ErrUnavailable
}
func TestControlledProfileStartsActualGuestPythonAtAuthenticatedBarrier(t *testing.T) {
	if _, e := os.Stat("/usr/bin/python3"); e != nil {
		t.Skip("controlled guest profile requires /usr/bin/python3")
	}
	old, _, a, cfg := controlledSetup(t)
	if e := old.Close(); e != nil {
		t.Fatal(e)
	}
	driver := &controlledGuestDriver{guestRuntime: guestRuntime{runtimeDriver: runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}, t: t}}
	b, e := hostapi.NewBridge(cfg, driver)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if o, e := b.Ensure(t.Context(), a); e != nil || o.State != "running" {
		t.Fatalf("controlled guest start: %v %v", o, e)
	}
	origin, e := b.ReadFile(t.Context(), a, &guestv1.ReadFileRequest{Path: "controlled-origin.txt"})
	if e != nil || string(origin.GetData()) != "counter=7\n" {
		t.Fatalf("controlled Python did not retain declared initial state: %v %v", origin, e)
	}
	if o, e := b.Observe(t.Context(), a); e != nil || o.State != "running" {
		t.Fatalf("authenticated guest barrier: %v %v", o, e)
	}
}

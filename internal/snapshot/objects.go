// Package snapshot stores fully materialized VM snapshots. It owns artifact
// integrity and lifetime pins, not sandbox lifecycle or checkpoint readiness in
// the controller. Scope and storage credentials come from a trusted caller.
package snapshot

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
)

var (
	ErrNotFound     = errors.New("snapshot object not found")
	ErrPrecondition = errors.New("snapshot object generation changed")
	ErrInvalid      = errors.New("invalid snapshot")
	ErrConflict     = errors.New("checkpoint already exists with different content or publication in progress")
	ErrIntegrity    = errors.New("snapshot integrity check failed")
	ErrIncompatible = errors.New("snapshot compatibility mismatch")
	ErrReferenced   = errors.New("snapshot is referenced")
	ErrDeleting     = errors.New("snapshot deletion has begun")
)

// Objects must provide linearizable conditional writes/deletes. An empty
// generation in Put means create-only, never unconditional overwrite. Get opens
// one immutable generation; subsequent replacement cannot change its bytes.
// Implementations must honor context deadlines on all operations.
type Objects interface {
	Get(context.Context, string) (io.ReadCloser, string, error)
	Put(context.Context, string, io.Reader, int64, string) (string, error)
	Delete(context.Context, string, string) error
	List(context.Context, string, string, int) ([]ObjectInfo, string, error)
}
type ObjectInfo struct {
	Key        string
	Generation string
}

var component = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func validKey(key string) bool {
	if key == "" || len(key) > 1024 {
		return false
	}
	for _, p := range strings.Split(key, "/") {
		if !component.MatchString(p) {
			return false
		}
	}
	return true
}

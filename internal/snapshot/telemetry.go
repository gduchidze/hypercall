package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

func manifestBytes(m Manifest) int64 {
	var size int64
	for _, artifact := range m.Artifacts {
		size += artifact.Size
	}
	return size
}

// Log only bounded outcome classes and trusted scoped identifiers. Errors can
// contain caller-local paths or credential-source details, so never log them.
func (s *Store) observe(ctx context.Context, event string, scope Scope, started time.Time, err error, size int64) {
	outcome := "error"
	switch {
	case err == nil:
		outcome = "success"
	case errors.Is(err, context.Canceled):
		outcome = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "deadline"
	case errors.Is(err, ErrInvalid):
		outcome = "invalid"
	case errors.Is(err, ErrIntegrity):
		outcome = "integrity"
	case errors.Is(err, ErrIncompatible):
		outcome = "incompatible"
	case errors.Is(err, ErrReferenced):
		outcome = "referenced"
	case errors.Is(err, ErrDeleting):
		outcome = "deleting"
	case errors.Is(err, ErrNotFound):
		outcome = "not_found"
	case errors.Is(err, ErrConflict), errors.Is(err, ErrPrecondition):
		outcome = "conflict"
	}
	attrs := []slog.Attr{slog.String("outcome", outcome), slog.Duration("duration", time.Since(started)), slog.Int64("artifact_bytes", size)}
	if scope.validate() == nil {
		attrs = append(attrs, slog.String("tenant_id", scope.TenantID), slog.String("project_id", scope.ProjectID), slog.String("checkpoint_id", scope.CheckpointID))
	}
	s.opts.Logger.LogAttrs(ctx, slog.LevelInfo, event, attrs...)
}

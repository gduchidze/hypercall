package guest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) ReadFile(ctx context.Context, req *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if err := s.ordinary(); err != nil {
		return nil, err
	}
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if !validPath(req.Path) {
		return nil, status.Error(codes.InvalidArgument, "workspace-relative path required")
	}
	file, err := s.root.OpenFile(req.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fileError(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fileError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, status.Error(codes.InvalidArgument, "only regular files can be read")
	}
	if info.Size() > MaxFileBytes {
		return nil, status.Error(codes.ResourceExhausted, "file exceeds transfer limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fileError(err)
	}
	if len(data) > MaxFileBytes {
		return nil, status.Error(codes.ResourceExhausted, "file exceeds transfer limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	s.logger.InfoContext(ctx, "guest file read", "bytes", len(data))
	return &guestv1.ReadFileResponse{Data: data}, nil
}

// WriteFile stages a complete bounded unary body, syncs it, and atomically
// renames within an opened parent directory. Failed staging never truncates an
// existing destination. Parent directories must already exist.
func (s *Server) WriteFile(ctx context.Context, req *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if err := s.ordinary(); err != nil {
		return nil, err
	}
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if !validPath(req.Path) || filepath.Base(req.Path) == "." {
		return nil, status.Error(codes.InvalidArgument, "workspace-relative file path required")
	}
	if len(req.Data) > MaxFileBytes {
		return nil, status.Error(codes.ResourceExhausted, "file exceeds transfer limit")
	}
	parent, err := s.root.OpenRoot(filepath.Dir(req.Path))
	if err != nil {
		return nil, fileError(err)
	}
	defer parent.Close()
	name := filepath.Base(req.Path)
	if info, err := parent.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return nil, status.Error(codes.InvalidArgument, "destination must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fileError(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, status.Error(codes.Internal, "file staging failed")
	}
	temp := ".hypercall-write-" + hex.EncodeToString(random)
	file, err := parent.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fileError(err)
	}
	defer parent.Remove(temp)
	defer file.Close()
	if !s.trustedLocal {
		if err := file.Chown(s.uid, s.gid); err != nil {
			return nil, fileError(err)
		}
	}
	if _, err := file.Write(req.Data); err != nil {
		return nil, fileError(err)
	}
	if err := file.Sync(); err != nil {
		return nil, fileError(err)
	}
	if err := file.Close(); err != nil {
		return nil, fileError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if !time.Now().Before(s.expiresAt) {
		return nil, status.Error(codes.Unauthenticated, "allocation session expired")
	}
	if err := parent.Rename(temp, name); err != nil {
		return nil, fileError(err)
	}
	s.logger.InfoContext(ctx, "guest file written", "bytes", len(req.Data))
	return &guestv1.WriteFileResponse{BytesWritten: uint64(len(req.Data))}, nil
}

func fileError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return status.Error(codes.ResourceExhausted, "workspace storage exhausted")
	}
	if errors.Is(err, os.ErrNotExist) {
		return status.Error(codes.NotFound, "workspace path not found")
	}
	if errors.Is(err, os.ErrPermission) {
		return status.Error(codes.PermissionDenied, "workspace path denied")
	}
	return status.Error(codes.InvalidArgument, "invalid workspace path or file operation")
}

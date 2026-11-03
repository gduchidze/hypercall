// Package api exposes the authenticated Hypercall control-plane boundary.
package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"

	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Principal struct {
	TenantID string   `json:"tenant_id"`
	Projects []string `json:"projects"`
}
type Authenticator struct{ keys map[[32]byte]Principal }
type principalKey struct{}

// NewAuthenticator accepts operator-provisioned API keys, each with explicit
// tenant/project grants. Keys are hashed in memory and never logged.
func NewAuthenticator(keys map[string]Principal) (*Authenticator, error) {
	a := &Authenticator{keys: map[[32]byte]Principal{}}
	if len(keys) == 0 {
		return nil, errors.New("at least one scoped API key is required")
	}
	for key, p := range keys {
		if len(key) < 32 || p.TenantID == "" || len(p.Projects) == 0 {
			return nil, errors.New("API keys require 32 bytes, tenant and explicit projects")
		}
		p.Projects = append([]string(nil), p.Projects...)
		for _, project := range p.Projects {
			if project == "" || project == "*" {
				return nil, errors.New("explicit nonempty project grants required")
			}
		}
		a.keys[sha256.Sum256([]byte(key))] = p
	}
	return a, nil
}
func (a *Authenticator) authenticate(ctx context.Context, values []string) (context.Context, error) {
	if a == nil || len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return nil, status.Error(codes.Unauthenticated, "one Bearer credential required")
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || strings.ContainsAny(token, " \r\n\t") {
		return nil, status.Error(codes.Unauthenticated, "invalid credential")
	}
	p, ok := a.keys[sha256.Sum256([]byte(token))]
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid credential")
	}
	return context.WithValue(ctx, principalKey{}, p), nil
}
func (a *Authenticator) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	ctx, err := a.authenticate(ctx, md.Get("authorization"))
	if err != nil {
		return nil, err
	}
	return next(ctx, req)
}

type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s authenticatedStream) Context() context.Context { return s.ctx }
func (a *Authenticator) stream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	ctx, err := a.authenticate(stream.Context(), md.Get("authorization"))
	if err != nil {
		return err
	}
	return next(srv, authenticatedStream{stream, ctx})
}
func scope(ctx context.Context, project string) (cp.Scope, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok {
		return cp.Scope{}, status.Error(codes.Unauthenticated, "authentication required")
	}
	if project == "" {
		return cp.Scope{}, status.Error(codes.InvalidArgument, "project_id required")
	}
	for _, grant := range p.Projects {
		if grant == project {
			return cp.Scope{TenantID: p.TenantID, ProjectID: project}, nil
		}
	}
	return cp.Scope{}, status.Error(codes.PermissionDenied, "project is not authorized")
}
func (a *Authenticator) HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := a.authenticate(r.Context(), r.Header.Values("Authorization"))
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":16,"message":"authentication required"}`))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

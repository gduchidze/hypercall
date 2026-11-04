package guest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type cloneSession struct {
	HarnessSession *harnessv1.HarnessSession   `json:"harness_session,omitempty"`
	Identity       *guestv1.AllocationIdentity `json:"identity"`
	Credential     string                      `json:"credential"`
	ExpiresAt      time.Time                   `json:"expires_at"`
	Entropy        string                      `json:"entropy"`
}

func (s cloneSession) CredentialHash() [32]byte { return sha256.Sum256([]byte(s.Credential)) }
func (s cloneSession) ValidateClone(parent *guestv1.AllocationIdentity, ctx context.Context) error {
	if s.Identity == nil || s.Identity.Epoch == 0 || s.Identity.TenantId != parent.TenantId || s.Identity.ProjectId != parent.ProjectId || s.Identity.SandboxId == parent.SandboxId || s.Identity.ExecutionId == parent.ExecutionId {
		return status.Error(codes.FailedPrecondition, "fresh scoped clone identity required")
	}
	for _, v := range []string{s.Identity.TenantId, s.Identity.ProjectId, s.Identity.SandboxId, s.Identity.ExecutionId} {
		if !identifier.MatchString(v) {
			return status.Error(codes.FailedPrecondition, "invalid clone identity")
		}
	}
	if len(s.Credential) < 32 || len(s.Credential) > 512 || strings.ContainsAny(s.Credential, " \t\r\n") {
		return status.Error(codes.FailedPrecondition, "invalid clone session")
	}
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return status.Error(codes.Unauthenticated, "fresh clone credential required")
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	want := s.CredentialHash()
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return status.Error(codes.Unauthenticated, "fresh clone credential required")
	}
	return nil
}

// The caller is authenticated from the freshly read private session disk before
// this function is reached. Never compare the sample against inherited guest UTC:
// a legitimate checkpoint may be restored much later than it was captured.
func refreshGuestClock(hostUnixNano int64, expires time.Time) error {
	if hostUnixNano <= 0 {
		return status.Error(codes.InvalidArgument, "positive host UTC sample required")
	}
	sampled := time.Unix(0, hostUnixNano)
	if !expires.After(sampled) || expires.Sub(sampled) > 24*time.Hour {
		return status.Error(codes.FailedPrecondition, "session expiry is invalid for supplied host UTC")
	}
	if err := setGuestClock(hostUnixNano); err != nil {
		return status.Error(codes.FailedPrecondition, "guest UTC refresh failed")
	}
	if !expires.After(time.Now()) || time.Until(expires) > 24*time.Hour {
		return status.Error(codes.FailedPrecondition, "renewed guest clock rejected session expiry")
	}
	return nil
}

// Package harness defines shared session integrity for the cooperative protocol.
// It owns no gateway routing, lifecycle decisions, or provider credentials.
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"google.golang.org/protobuf/proto"
)

const Protocol = "hypercall.harness.v1"

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func Digest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}
func ValidDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") {
		return false
	}
	b, e := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}
func MessageHash(m proto.Message) (string, error) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return "", errors.New("protobuf message required")
	}
	b, e := (proto.MarshalOptions{Deterministic: true}).Marshal(m)
	if e != nil {
		return "", e
	}
	return Digest(b), nil
}
func validateFields(s *harnessv1.HarnessSession) error {
	if s == nil || s.ProtocolVersion != Protocol || s.Epoch < 1 || !ValidDigest(s.RouteDigest) || !ValidDigest(s.ModelDigest) {
		return errors.New("invalid harness session protocol/epoch/digests")
	}
	for _, v := range []string{s.TenantId, s.ProjectId, s.BranchId, s.SandboxId, s.ExecutionId, s.CallNamespace} {
		if !identifier.MatchString(v) {
			return errors.New("invalid harness session identity")
		}
	}
	if (s.ParentCheckpointId == "") != (s.InheritedBarrier == nil) {
		return errors.New("inherited checkpoint and barrier required together")
	}
	if s.InheritedBarrier != nil {
		b := s.InheritedBarrier
		if !identifier.MatchString(s.ParentCheckpointId) || b.ProtocolVersion != Protocol || b.Step < 0 || b.EventSequence < 1 || b.SourceEpoch < 1 || !identifier.MatchString(b.SourceBranchId) || !identifier.MatchString(b.SourceExecutionId) || !ValidDigest(b.StateDigest) || !ValidDigest(b.SourceSessionHash) {
			return errors.New("invalid inherited semantic barrier")
		}
	}
	return nil
}
func SealSession(s *harnessv1.HarnessSession) (*harnessv1.HarnessSession, error) {
	if e := validateFields(s); e != nil {
		return nil, e
	}
	sealed := proto.Clone(s).(*harnessv1.HarnessSession)
	sealed.SessionHash = ""
	hash, e := MessageHash(sealed)
	if e != nil {
		return nil, e
	}
	sealed.SessionHash = hash
	return sealed, nil
}
func ValidateSession(s *harnessv1.HarnessSession) error {
	sealed, e := SealSession(s)
	if e != nil {
		return e
	}
	if s.SessionHash != sealed.SessionHash {
		return errors.New("harness session configuration hash mismatch")
	}
	return nil
}

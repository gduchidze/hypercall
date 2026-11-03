package snapshot

import (
	"context"
	"errors"
	"sort"
	"time"
)

func (s *Store) readLifetime(ctx context.Context, scope Scope) (lifetime, string, error) {
	var st lifetime
	g, _, err := s.getJSON(ctx, scope.prefix()+"/lifetime", &st)
	if err != nil {
		return st, "", err
	}
	if st.Version != 1 || !component.MatchString(st.UploadID) || (st.Phase != "ready" && st.Phase != "publishing" && st.Phase != "deleting") || len(st.References) > 1024 {
		return st, "", ErrIntegrity
	}
	for i, r := range st.References {
		if !component.MatchString(r) || (i > 0 && st.References[i-1] >= r) {
			return st, "", ErrIntegrity
		}
	}
	if st.Phase != "ready" && len(st.References) > 0 {
		return st, "", ErrIntegrity
	}
	return st, g, nil
}

// AcquireReference durably pins artifacts. References never expire: an active VM
// can retain its memory backing beyond any controller lease. Allocation recovery
// is responsible for releasing pins only after it proves the VM has stopped.
func (s *Store) AcquireReference(ctx context.Context, scope Scope, ref string) (resultErr error) {
	started := time.Now()
	defer func() { s.observe(ctx, "snapshot_pin_acquire", scope, started, resultErr, 0) }()
	return s.changeReference(ctx, scope, ref, true)
}
func (s *Store) ReleaseReference(ctx context.Context, scope Scope, ref string) (resultErr error) {
	started := time.Now()
	defer func() { s.observe(ctx, "snapshot_pin_release", scope, started, resultErr, 0) }()
	return s.changeReference(ctx, scope, ref, false)
}
func (s *Store) changeReference(ctx context.Context, scope Scope, ref string, acquire bool) error {
	ctx, cancel := s.operation(ctx)
	defer cancel()
	if scope.validate() != nil || !component.MatchString(ref) {
		return ErrInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, g, err := s.readLifetime(ctx, scope)
		if !acquire && errors.Is(err, ErrNotFound) {
			// Recovery can release a pin whose acquisition never happened.
			// A missing lifetime record cannot own any persisted references.
			return nil
		}
		if err != nil {
			return err
		}
		if st.Phase == "deleting" {
			if !acquire {
				return nil
			}
			return ErrDeleting
		}
		if st.Phase != "ready" {
			return ErrConflict
		}
		i := sort.SearchStrings(st.References, ref)
		exists := i < len(st.References) && st.References[i] == ref
		if acquire == exists {
			return nil
		}
		if acquire {
			if len(st.References) >= 1024 {
				return ErrInvalid
			}
			st.References = append(st.References, ref)
			sort.Strings(st.References)
		} else {
			st.References = append(st.References[:i], st.References[i+1:]...)
		}
		_, err = s.putJSON(ctx, scope.prefix()+"/lifetime", st, g)
		if errors.Is(err, ErrPrecondition) {
			continue
		}
		return err
	}
}

// DeleteUnreferenced atomically fences new references before removing artifacts.
// Its durable tombstone is deliberately retained, preventing checkpoint-ID reuse
// and allowing cleanup to resume after a process dies during deletion.
func (s *Store) DeleteUnreferenced(ctx context.Context, scope Scope) (resultErr error) {
	started := time.Now()
	defer func() { s.observe(ctx, "snapshot_delete", scope, started, resultErr, 0) }()
	ctx, cancel := s.operation(ctx)
	defer cancel()
	if scope.validate() != nil {
		return ErrInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, g, err := s.readLifetime(ctx, scope)
		if err != nil {
			return err
		}
		if st.Phase == "deleting" {
			if time.Now().Before(st.DeleteAfter) {
				return ErrConflict
			}
			return s.removeObjects(ctx, scope)
		}
		if st.Phase != "ready" {
			return ErrConflict
		}
		if len(st.References) > 0 {
			return ErrReferenced
		}
		st.Phase = "deleting"
		st.DeleteAfter = time.Time{}
		_, err = s.putJSON(ctx, scope.prefix()+"/lifetime", st, g)
		if errors.Is(err, ErrPrecondition) {
			continue
		}
		if err != nil {
			return err
		}
		return s.removeObjects(ctx, scope)
	}
}
func (s *Store) removeObjects(ctx context.Context, scope Scope) error {
	cursor := ""
	for {
		objects, next, err := s.objects.List(ctx, scope.prefix()+"/", cursor, 100)
		if err != nil {
			return err
		}
		for _, object := range objects {
			if object.Key == scope.prefix()+"/lifetime" {
				continue
			}
			if err = s.objects.Delete(ctx, object.Key, object.Generation); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// RecoverExpired fences an abandoned publishing lease and collects its objects.
// It never promotes partial uploads. Checkpoint IDs are immutable, so an aborted
// checkpoint is retried with a new ID. The caller enumerates its admitted upload
// journal, keeping recovery bounded to a trusted scope. Repeat after crashes or
// delayed provider completion; tombstones remain permanently unavailable.
func (s *Store) RecoverExpired(ctx context.Context, scope Scope) (resultErr error) {
	started := time.Now()
	defer func() { s.observe(ctx, "snapshot_recover", scope, started, resultErr, 0) }()
	ctx, cancel := s.operation(ctx)
	defer cancel()
	if scope.validate() != nil {
		return ErrInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, g, err := s.readLifetime(ctx, scope)
		if err != nil {
			return err
		}
		if st.Phase == "ready" {
			return nil
		}
		if st.Phase == "deleting" {
			if time.Now().Before(st.DeleteAfter) {
				return ErrConflict
			}
			return s.removeObjects(ctx, scope)
		}
		if time.Now().Before(st.LeaseUntil) {
			return ErrConflict
		}
		st.Phase = "deleting"
		st.DeleteAfter = st.LeaseUntil.Add(s.opts.OperationTimeout)
		_, err = s.putJSON(ctx, scope.prefix()+"/lifetime", st, g)
		if errors.Is(err, ErrPrecondition) {
			continue
		}
		if err != nil {
			return err
		}
		if time.Now().Before(st.DeleteAfter) {
			return ErrConflict
		}
		return s.removeObjects(ctx, scope)
	}
}

// RecoveryGrace bounds retention of a host capture intent with an ambiguously
// missing lifetime record. Publication leases start at Publish entry, including
// source hashing; retain the intent this long from before calling Publish, then
// retry RecoverExpired. All participating hosts must use compatible timeouts.
func (s *Store) RecoveryGrace() time.Duration {
	return s.opts.StagingTTL + s.opts.OperationTimeout
}

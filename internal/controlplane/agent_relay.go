package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/harness"
)

// AgentBackend is the authenticated host-to-guest mailbox adapter. The controller
// remains the lifecycle owner; this adapter carries bounded data, never routes.
type AgentBackend interface {
	AgentPoll(context.Context, Assignment) (AgentPoll, error)
	AgentDeliver(context.Context, Assignment, AgentDelivery) error
}

func loadAssignment(ctx context.Context, tx *sql.Tx, scope Scope, branch string) (Assignment, error) {
	a := Assignment{Scope: scope, BranchID: branch}
	e := tx.QueryRowContext(ctx, `SELECT execution_id,sandbox_id,epoch,lease_until FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, branch).Scan(&a.ExecutionID, &a.SandboxID, &a.Epoch, &a.LeaseUntil)
	if e != nil {
		return a, e
	}
	a.Run, e = loadRun(ctx, tx, scope, branch)
	if e != nil {
		return a, e
	}
	e = fillAgentAssignment(ctx, tx, &a)
	return a, e
}
func (c *Controller) AgentTick(ctx context.Context) error {
	backend, ok := c.Backend.(AgentBackend)
	if !ok {
		return nil
	}
	type target struct {
		scope    Scope
		id       string
		complete bool
	}
	all := []target{}
	for attempt := 0; attempt < 2; attempt++ {
		rows, e := c.Store.db.QueryContext(ctx, `SELECT tenant_id,project_id,id,trace_complete FROM branches WHERE desired IN ('running','suspended') AND observed IN ('running','suspending') AND lease_until>clock_timestamp() AND harness_session IS NOT NULL AND (tenant_id,project_id,id)>($1,$2,$3) ORDER BY tenant_id,project_id,id LIMIT 32`, c.agentAfter[0], c.agentAfter[1], c.agentAfter[2])
		if e != nil {
			return e
		}
		for rows.Next() {
			var t target
			if e = rows.Scan(&t.scope.TenantID, &t.scope.ProjectID, &t.id, &t.complete); e != nil {
				rows.Close()
				return e
			}
			all = append(all, t)
		}
		if e = rows.Close(); e != nil {
			return e
		}
		if len(all) > 0 {
			break
		}
		c.agentAfter = [3]string{}
	}
	var failures []error
	for _, t := range all {
		c.agentAfter = [3]string{t.scope.TenantID, t.scope.ProjectID, t.id}
		tx, e := c.Store.db.BeginTx(ctx, nil)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		a, e := loadAssignment(ctx, tx, t.scope, t.id)
		if e != nil {
			tx.Rollback()
			failures = append(failures, e)
			continue
		}
		if e = tx.Commit(); e != nil {
			failures = append(failures, e)
			continue
		}
		if e = c.relayAgent(ctx, backend, a, t.complete); e != nil {
			failures = append(failures, fmt.Errorf("agent branch %s: %w", t.id, e))
		}
	}
	return errors.Join(failures...)
}
func (c *Controller) relayAgent(ctx context.Context, backend AgentBackend, a Assignment, complete bool) error {
	pollCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	poll, e := backend.AgentPoll(pollCtx, a)
	cancel()
	if e != nil {
		return e
	}
	delivery := AgentDelivery{SessionHash: a.HarnessSession.SessionHash}
	switch {
	case poll.SessionAck != "":
		if e = c.Store.AcknowledgeAgentSession(ctx, a, poll.SessionAck); e != nil {
			return e
		}
		delivery.SessionAck = poll.SessionAck
	case poll.Request != nil:
		result, e := (&Service{store: c.Store, backend: c.Backend}).ProcessAgentRequest(ctx, a, *poll.Request)
		if e != nil {
			return e
		}
		if result.State == "pending" {
			return nil
		}
		delivery.Result = &result
		r := poll.Request
		delivery.RequestHash, e = harness.MessageHash(&harnessv1.AgentRequest{Id: r.ID, Kind: r.Kind, Step: r.Step, Position: r.Position, Name: r.Name, Payload: r.Payload, PayloadDigest: r.PayloadDigest, SessionHash: r.SessionHash})
		if e != nil {
			return e
		}
	case poll.Barrier != nil:
		sealed, e := c.Store.SealAgentBarrier(ctx, a, *poll.Barrier)
		if e != nil {
			return e
		}
		delivery.SealedBarrier = &sealed
		b := poll.Barrier
		delivery.RequestHash, e = harness.MessageHash(&harnessv1.AgentBarrier{Step: b.Step, EventSequence: b.EventSequence, StateDigest: b.StateDigest, ProtocolVersion: b.ProtocolVersion, SessionHash: b.SessionHash})
		if e != nil {
			return e
		}
	default:
		if complete {
			return c.Store.completeAgentExecution(ctx, a)
		}
		return nil
	}
	deliverCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	e = backend.AgentDeliver(deliverCtx, a, delivery)
	cancel()
	if e != nil {
		return e
	}
	if delivery.Result != nil && delivery.Result.State == "finalized" {
		return c.Store.completeAgentExecution(ctx, a)
	}
	return nil
}

// Finalization closes the action gate first; VM cleanup is requested only after
// delivery or an idle mailbox confirms the terminal handshake. Stop is durable.
func (s *Store) completeAgentExecution(ctx context.Context, a Assignment) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	v, e := lockAgent(ctx, tx, a)
	if e != nil {
		return e
	}
	if v.gate != "finalized" {
		return failure("failed_precondition", "agent did not commit finalization")
	}
	var revision int64
	e = tx.QueryRowContext(ctx, `UPDATE branches SET desired='stopped',revision=revision+1 WHERE tenant_id=$1 AND project_id=$2 AND id=$3 RETURNING revision`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID).Scan(&revision)
	if e != nil {
		return e
	}
	op := operation(a.Scope, a.BranchID, "complete")
	if e = insertResource(ctx, tx, op); e != nil {
		return e
	}
	if e = enqueue(ctx, tx, a.Scope, a.BranchID, op, "complete", revision); e != nil {
		return e
	}
	return tx.Commit()
}

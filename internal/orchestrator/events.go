package orchestrator

import (
	"context"
	"errors"

	"github.com/hstern/fj-bellows/internal/control/events"
)

// Stable attribute keys for emitted events. Defined as consts so that
// adding/renaming an attribute touches one place and lint's goconst rule
// doesn't keep tripping on repeated literals.
const (
	attrID         = "id"
	attrIP         = "ip"
	attrHandle     = "handle"
	attrState      = "state"
	attrRunnerUUID = "runner_uuid"
	attrName       = "name"
	attrUUID       = "uuid"
	attrCaller     = "caller"
)

// emit publishes a state-transition event to the orchestrator's event bus
// when one is wired. Tests that construct *Orchestrator by hand without
// calling New get a nil bus and emit becomes a no-op — keep this null-safe.
func (o *Orchestrator) emit(typ string, attrs map[string]string) {
	if o.events == nil {
		return
	}
	o.events.Publish(events.Event{At: o.now(), Type: typ, Attrs: attrs})
}

// Subscribe returns a channel of state-transition events plus a cancel func.
// The channel is closed when the caller cancels OR when the orchestrator
// drops the subscriber for slow consumption. Receivers should range until
// close.
func (o *Orchestrator) Subscribe() (<-chan events.Event, func()) {
	if o.events == nil {
		ch := make(chan events.Event)
		close(ch)
		return ch, func() {}
	}
	return o.events.Subscribe()
}

// Kick drives a synchronous reconcile from out of band (the control plane's
// Reconcile RPC). Returns the same ReconcileResult the in-band ticker would
// produce; preserves the single-writer property because the request is
// served from the Run goroutine's select.
func (o *Orchestrator) Kick(ctx context.Context) (ReconcileResult, error) {
	if o.kick == nil {
		return ReconcileResult{}, errors.New("orchestrator not running (no kick channel)")
	}
	resultCh := make(chan ReconcileResult, 1)
	select {
	case o.kick <- kickReq{kind: kickReconcile, reconcile: resultCh}:
	case <-ctx.Done():
		return ReconcileResult{}, ctx.Err()
	}
	select {
	case r := <-resultCh:
		return r, nil
	case <-ctx.Done():
		return ReconcileResult{}, ctx.Err()
	}
}

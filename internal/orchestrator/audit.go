package orchestrator

import "context"

// auditCallerKey is the unexported key type the orchestrator uses to thread
// an audit-caller identity through a context. Unexported per Go convention so
// no other package can collide with it.
type auditCallerKey struct{}

// WithAuditCaller returns a child context carrying caller as the audit
// identity for any force-* RPC invoked downstream. The control plane handler
// builds the identity from the Connect request peer (and, when set, the
// bearer-token presence) and threads it through to the orchestrator's
// ForceReap / ForceProvision calls so every audit log line records who
// triggered it.
//
// The caller string is informational — short and human-readable. A typical
// value is "peer=10.0.0.5:54312 token" or "loopback". Don't put secrets here
// (it ends up in slog output).
func WithAuditCaller(ctx context.Context, caller string) context.Context {
	if caller == "" {
		return ctx
	}
	return context.WithValue(ctx, auditCallerKey{}, caller)
}

// auditCallerFromCtx reads the audit identity threaded into ctx by
// WithAuditCaller. Returns "loopback" when nothing is set — the daemon's
// default-bind is 127.0.0.1, so the absence of a value is itself a
// meaningful signal ("nobody set this; it came in on the unauthenticated
// loopback path").
func auditCallerFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(auditCallerKey{}).(string); ok && v != "" {
		return v
	}
	return "loopback"
}

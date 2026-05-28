// Package agent implements fjbagent — the small daemon that runs on every
// fj-bellows worker and cache VM. It exposes ConnectRPC services the
// orchestrator dials into (Health here; Exec lands in FJB-93). Auth is a
// per-deployment shared secret on the Authorization header; the wire is
// plain HTTP/2 cleartext on top of the WG fabric.
package agent

import (
	"context"
	"os"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/hstern/fj-bellows/gen/fjbellows/agent/v1"
)

// Handler is the AgentService implementation. It holds process-scope
// facts (start time, hostname, build version) that Health returns. Field
// values are captured once at NewHandler time so a slow os.Hostname call
// doesn't block every RPC.
type Handler struct {
	buildVersion string
	startedAt    time.Time
	hostname     string
}

// NewHandler captures the process-scope health facts. version is the
// build's main.version string ("dev" when unstamped); now is the start
// time anchor (taken at NewHandler so a delayed Run still reports an
// honest start moment).
func NewHandler(version string, now time.Time) *Handler {
	h, err := os.Hostname()
	if err != nil {
		// Hostname lookup is best-effort. An empty string in the response
		// is a clear "we didn't get it" rather than a fabricated guess.
		h = ""
	}
	return &Handler{
		buildVersion: version,
		startedAt:    now,
		hostname:     h,
	}
}

// Health returns the readiness snapshot. Always succeeds; the existence
// of the response is itself the "agent is up" signal the orchestrator
// readiness gate watches for.
func (h *Handler) Health(_ context.Context, _ *connect.Request[agentv1.HealthRequest]) (*connect.Response[agentv1.HealthResponse], error) {
	return connect.NewResponse(&agentv1.HealthResponse{
		Ready:        true,
		BuildVersion: h.buildVersion,
		StartedAt:    timestamppb.New(h.startedAt),
		Hostname:     h.hostname,
	}), nil
}

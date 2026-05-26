package control

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
	"github.com/hstern/fj-bellows/internal/control/logbus"
	"github.com/hstern/fj-bellows/internal/orchestrator"
)

// defaultLogHistoryLines is the replay-on-connect size when a StreamLogs
// client doesn't ask for a specific count. A few hundred lines is enough
// for "what just happened?" without dumping the whole ring buffer.
const defaultLogHistoryLines = 100

// apiHandler adapts a Backend to the generated ConnectRPC service surface.
// Keeping protobuf imports in this file (and not in the orchestrator package)
// means the orchestrator stays free of generated-code coupling.
type apiHandler struct {
	controlv1connect.UnimplementedControlServiceHandler
	b Backend
	// enableWrites gates the mutating ForceReap / ForceProvision RPCs.
	// When false, those RPCs short-circuit to CodePermissionDenied before
	// touching the backend. Read-only RPCs ignore it entirely.
	enableWrites bool
}

// errWritesDisabled is the response body for force-* RPCs when the
// -enable-control-writes flag is unset. Operator-facing: tells them which
// flag to flip.
var errWritesDisabled = errors.New("control writes not enabled (set -enable-control-writes)")

func (h *apiHandler) Health(
	ctx context.Context,
	_ *connect.Request[controlv1.HealthRequest],
) (*connect.Response[controlv1.HealthResponse], error) {
	s := h.b.Health(ctx)
	return connect.NewResponse(&controlv1.HealthResponse{
		Healthy:            s.Healthy,
		LastTickAt:         tsOrNil(s.LastTickAt),
		LastProviderListAt: tsOrNil(s.LastProviderListAt),
		LastForgejoPollAt:  tsOrNil(s.LastForgejoPollAt),
		Paused:             s.Paused,
	}), nil
}

func (h *apiHandler) ListWorkers(
	_ context.Context,
	_ *connect.Request[controlv1.ListWorkersRequest],
) (*connect.Response[controlv1.ListWorkersResponse], error) {
	view := h.b.PoolSnapshot()
	workers := make([]*controlv1.Worker, 0, len(view))
	for _, w := range view {
		workers = append(workers, &controlv1.Worker{
			InstanceId:     w.InstanceID,
			State:          w.State,
			Ip:             w.IP,
			CreatedAt:      tsOrNil(w.CreatedAt),
			LastBusy:       tsOrNil(w.LastBusy),
			CurrentJob:     w.CurrentJob,
			PaidHourEndAt:  tsOrNil(w.PaidHourEndAt),
			ReapEligibleAt: tsOrNil(w.ReapEligibleAt),
			BillingModel:   w.BillingModel,
		})
	}
	return connect.NewResponse(&controlv1.ListWorkersResponse{Workers: workers}), nil
}

func (h *apiHandler) GetCache(
	ctx context.Context,
	_ *connect.Request[controlv1.GetCacheRequest],
) (*connect.Response[controlv1.GetCacheResponse], error) {
	s := h.b.CacheStatus(ctx)
	if s == nil {
		return connect.NewResponse(&controlv1.GetCacheResponse{Present: false}), nil
	}
	return connect.NewResponse(&controlv1.GetCacheResponse{
		Present:         s.Present,
		AdoptedExisting: s.AdoptedExisting,
		LinodeId:        int64(s.LinodeID),
		VpcIp:           s.VPCIP,
		BucketRegion:    s.BucketRegion,
		BucketLabel:     s.BucketLabel,
		VmState:         s.VMState,
	}), nil
}

func (h *apiHandler) Reconcile(
	ctx context.Context,
	_ *connect.Request[controlv1.ReconcileRequest],
) (*connect.Response[controlv1.ReconcileResponse], error) {
	r, err := h.b.Kick(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&controlv1.ReconcileResponse{
		//nolint:gosec // counts come from in-process int counters; can't overflow int32 in practice
		Provisioned: int32(r.Provisioned),
		//nolint:gosec // see above
		Dispatched: int32(r.Dispatched),
		//nolint:gosec // see above
		Reaped: int32(r.Reaped),
		//nolint:gosec // see above
		Adopted: int32(r.Adopted),
		//nolint:gosec // see above
		Dropped: int32(r.Dropped),
		Errors:  r.Errors,
	}), nil
}

func (h *apiHandler) StreamEvents(
	ctx context.Context,
	_ *connect.Request[controlv1.StreamEventsRequest],
	stream *connect.ServerStream[controlv1.StreamEventsResponse],
) error {
	ch, cancel := h.b.Subscribe()
	defer cancel()
	// Send a sentinel event immediately so the client's call returns
	// without waiting for the first real event. Connect server-streaming
	// only writes response headers on the first Send; without this, a
	// quiet daemon would make the client appear to hang on Open.
	if err := stream.Send(&controlv1.StreamEventsResponse{
		At:   tsOrNil(time.Now()),
		Type: "stream_opened",
	}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				// Bus dropped us for slow consumption.
				return connect.NewError(connect.CodeResourceExhausted,
					errors.New("stream subscriber dropped: client too slow"))
			}
			if err := stream.Send(&controlv1.StreamEventsResponse{
				At:    tsOrNil(ev.At),
				Type:  ev.Type,
				Attrs: ev.Attrs,
			}); err != nil {
				return err
			}
		}
	}
}

func (h *apiHandler) StreamLogs(
	ctx context.Context,
	req *connect.Request[controlv1.StreamLogsRequest],
	stream *connect.ServerStream[controlv1.StreamLogsResponse],
) error {
	filter := logbus.Filter{
		InstanceID: req.Msg.InstanceId,
		Handle:     req.Msg.Handle,
	}
	// Pick replay size: explicit request wins (capped at ring capacity);
	// otherwise replay defaultLogHistoryLines.
	history := int(req.Msg.HistoryLines)
	history = max(history, 0)
	if req.Msg.HistoryLines == 0 {
		history = defaultLogHistoryLines
	}
	history = min(history, logbus.HistoryCapacity)

	// Subscribe BEFORE fetching history so any record published between
	// the history snapshot and the first Recv lands in the subscriber
	// buffer rather than being dropped.
	ch, cancel := h.b.SubscribeLogs(filter)
	defer cancel()

	// Sentinel: makes the open call return immediately on a quiet daemon
	// (Connect server-streaming only writes response headers on first
	// Send). Same convention as StreamEvents.
	if err := stream.Send(&controlv1.StreamLogsResponse{
		At: tsOrNil(time.Now()),
	}); err != nil {
		return err
	}

	if history > 0 {
		for _, r := range h.b.LogHistory(history, filter) {
			if err := stream.Send(logRecordToResponse(r)); err != nil {
				return err
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case r, ok := <-ch:
			if !ok {
				return connect.NewError(connect.CodeResourceExhausted,
					errors.New("stream subscriber dropped: client too slow"))
			}
			if err := stream.Send(logRecordToResponse(r)); err != nil {
				return err
			}
		}
	}
}

func logRecordToResponse(r logbus.Record) *controlv1.StreamLogsResponse {
	return &controlv1.StreamLogsResponse{
		At:      tsOrNil(r.At),
		Level:   r.Level.String(),
		Message: r.Message,
		Attrs:   r.Attrs,
	}
}

func (h *apiHandler) ForceReap(
	ctx context.Context,
	req *connect.Request[controlv1.ForceReapRequest],
) (*connect.Response[controlv1.ForceReapResponse], error) {
	if !h.enableWrites {
		return nil, connect.NewError(connect.CodePermissionDenied, errWritesDisabled)
	}
	id := req.Msg.InstanceId
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("instance_id is required"))
	}
	ctx = orchestrator.WithAuditCaller(ctx, auditCaller(req))
	if err := h.b.ForceReap(ctx, id); err != nil {
		// "not in pool" is a 4xx (the operator named something that
		// doesn't exist); other failures are 5xx.
		if strings.Contains(err.Error(), "not in pool") || strings.Contains(err.Error(), "vanished from pool") {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&controlv1.ForceReapResponse{}), nil
}

func (h *apiHandler) ForceProvision(
	ctx context.Context,
	req *connect.Request[controlv1.ForceProvisionRequest],
) (*connect.Response[controlv1.ForceProvisionResponse], error) {
	if !h.enableWrites {
		return nil, connect.NewError(connect.CodePermissionDenied, errWritesDisabled)
	}
	ctx = orchestrator.WithAuditCaller(ctx, auditCaller(req))
	id, err := h.b.ForceProvision(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&controlv1.ForceProvisionResponse{InstanceId: id}), nil
}

func (h *apiHandler) Pause(
	ctx context.Context,
	req *connect.Request[controlv1.PauseRequest],
) (*connect.Response[controlv1.PauseResponse], error) {
	if !h.enableWrites {
		return nil, connect.NewError(connect.CodePermissionDenied, errWritesDisabled)
	}
	ctx = orchestrator.WithAuditCaller(ctx, auditCaller(req))
	h.b.Pause(ctx)
	return connect.NewResponse(&controlv1.PauseResponse{}), nil
}

func (h *apiHandler) Resume(
	ctx context.Context,
	req *connect.Request[controlv1.ResumeRequest],
) (*connect.Response[controlv1.ResumeResponse], error) {
	if !h.enableWrites {
		return nil, connect.NewError(connect.CodePermissionDenied, errWritesDisabled)
	}
	ctx = orchestrator.WithAuditCaller(ctx, auditCaller(req))
	h.b.Resume(ctx)
	return connect.NewResponse(&controlv1.ResumeResponse{}), nil
}

// auditCaller builds a short, log-safe identity string from the Connect
// request: the peer address always, plus a "token" marker when the
// Authorization header carries a bearer token (we don't decode it — the
// header's presence is the signal). Format example: "peer=127.0.0.1:54312"
// or "peer=10.0.0.5:60000 token". Loopback peers also get the explicit
// peer= prefix so the operator can distinguish "someone hit /healthz over
// loopback" from "nothing set this at all".
func auditCaller[T any](req *connect.Request[T]) string {
	parts := make([]string, 0, 2)
	if p := req.Peer().Addr; p != "" {
		parts = append(parts, "peer="+p)
	}
	if h := req.Header().Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		parts = append(parts, "token")
	}
	return strings.Join(parts, " ")
}

// tsOrNil emits a Timestamp only for non-zero times; zero stays nil so the
// wire form omits the field instead of advertising 1970-01-01.
func tsOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

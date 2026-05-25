package control

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
)

// apiHandler adapts a Backend to the generated ConnectRPC service surface.
// Keeping protobuf imports in this file (and not in the orchestrator package)
// means the orchestrator stays free of generated-code coupling.
type apiHandler struct {
	controlv1connect.UnimplementedControlServiceHandler
	b Backend
}

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
			InstanceId: w.InstanceID,
			State:      w.State,
			Ip:         w.IP,
			CreatedAt:  tsOrNil(w.CreatedAt),
			LastBusy:   tsOrNil(w.LastBusy),
			CurrentJob: w.CurrentJob,
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

// tsOrNil emits a Timestamp only for non-zero times; zero stays nil so the
// wire form omits the field instead of advertising 1970-01-01.
func tsOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

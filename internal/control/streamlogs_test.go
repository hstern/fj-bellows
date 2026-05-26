package control_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/internal/control/logbus"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

const testInstance = "vm-1"

func TestStreamLogs_SentinelThenLiveRecords(t *testing.T) {
	bus := logbus.New()
	be := &mockctl.Backend{}
	be.SetSubscribeLogs(bus.SubscribeFiltered)
	be.SetLogHistory(bus.History)

	_, client := newTestServer(t, be)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamLogs(ctx, connect.NewRequest(&controlv1.StreamLogsRequest{
		// HistoryLines=0 still replays defaultLogHistoryLines=100, but the
		// bus is empty so replay yields nothing.
	}))
	if err != nil {
		t.Fatalf("StreamLogs open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// First message: sentinel (empty level/message).
	if !stream.Receive() {
		t.Fatalf("sentinel: %v", stream.Err())
	}
	if got := stream.Msg().Message; got != "" {
		t.Fatalf("sentinel message: want empty got %q", got)
	}
	if got := stream.Msg().Level; got != "" {
		t.Fatalf("sentinel level: want empty got %q", got)
	}

	// Now publish.
	bus.Publish(logbus.Record{
		At:      time.Now(),
		Level:   slog.LevelInfo,
		Message: "worker ready",
		Attrs:   map[string]string{"id": testInstance},
	})
	bus.Publish(logbus.Record{
		At:      time.Now(),
		Level:   slog.LevelError,
		Message: "boom",
		Attrs:   map[string]string{"handle": "h1"},
	})

	if !stream.Receive() {
		t.Fatalf("first live Receive: %v", stream.Err())
	}
	if stream.Msg().Message != "worker ready" {
		t.Fatalf("first: %q", stream.Msg().Message)
	}
	if stream.Msg().Level != "INFO" {
		t.Fatalf("first level: %q", stream.Msg().Level)
	}
	if stream.Msg().Attrs["id"] != testInstance {
		t.Fatalf("first attrs: %+v", stream.Msg().Attrs)
	}

	if !stream.Receive() {
		t.Fatalf("second live Receive: %v", stream.Err())
	}
	if stream.Msg().Message != "boom" {
		t.Fatalf("second: %q", stream.Msg().Message)
	}
	if stream.Msg().Level != "ERROR" {
		t.Fatalf("second level: %q", stream.Msg().Level)
	}
}

func TestStreamLogs_ReplaysHistory(t *testing.T) {
	bus := logbus.New()
	// Pre-populate history before any subscriber connects.
	for i := range 5 {
		bus.Publish(logbus.Record{
			Level:   slog.LevelInfo,
			Message: "old",
			Attrs:   map[string]string{"i": itoaTest(i)},
		})
	}

	be := &mockctl.Backend{}
	be.SetSubscribeLogs(bus.SubscribeFiltered)
	be.SetLogHistory(bus.History)

	_, client := newTestServer(t, be)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamLogs(ctx, connect.NewRequest(&controlv1.StreamLogsRequest{
		HistoryLines: 3, // last 3 of 5
	}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Skip sentinel.
	if !stream.Receive() {
		t.Fatalf("sentinel: %v", stream.Err())
	}

	// Should replay last 3 records: i=2,3,4.
	for _, wantI := range []string{"2", "3", "4"} {
		if !stream.Receive() {
			t.Fatalf("replay Receive: %v", stream.Err())
		}
		if got := stream.Msg().Attrs["i"]; got != wantI {
			t.Fatalf("replay i: want %s got %s", wantI, got)
		}
	}
}

func TestStreamLogs_FilterScopesByInstance(t *testing.T) {
	bus := logbus.New()
	be := &mockctl.Backend{}
	be.SetSubscribeLogs(bus.SubscribeFiltered)
	be.SetLogHistory(bus.History)

	_, client := newTestServer(t, be)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamLogs(ctx, connect.NewRequest(&controlv1.StreamLogsRequest{
		InstanceId: testInstance,
	}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		t.Fatalf("sentinel: %v", stream.Err())
	}

	bus.Publish(logbus.Record{Message: "other", Attrs: map[string]string{"id": "vm-2"}})
	bus.Publish(logbus.Record{Message: "mine", Attrs: map[string]string{"id": testInstance}})

	if !stream.Receive() {
		t.Fatalf("Receive: %v", stream.Err())
	}
	if got := stream.Msg().Message; got != "mine" {
		t.Fatalf("filter leaked: got %q", got)
	}
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

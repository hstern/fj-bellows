package logbus_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/control/logbus"
)

// TestHandler_DelegatesToNextAndPublishesToBus is the delegation pin: every
// record must reach both the next handler (stderr text output) AND the bus.
func TestHandler_DelegatesToNextAndPublishesToBus(t *testing.T) {
	var buf bytes.Buffer
	bus := logbus.New()
	h := logbus.NewHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), bus)
	log := slog.New(h)

	sub, cancel := bus.Subscribe()
	defer cancel()

	log.Info("hello", "id", testInstance, "n", 42)

	// Bus must receive it.
	select {
	case r := <-sub:
		if r.Message != "hello" {
			t.Fatalf("bus message: want hello got %q", r.Message)
		}
		if r.Level != slog.LevelInfo {
			t.Fatalf("bus level: want INFO got %v", r.Level)
		}
		if r.Attrs["id"] != testInstance {
			t.Fatalf("bus attrs id: %+v", r.Attrs)
		}
		if r.Attrs["n"] != "42" {
			t.Fatalf("bus attrs n: %+v", r.Attrs)
		}
	case <-time.After(time.Second):
		t.Fatal("bus did not receive")
	}

	// Next handler must also have seen it (stderr text output).
	if !strings.Contains(buf.String(), `msg=hello`) || !strings.Contains(buf.String(), `id=vm-1`) {
		t.Fatalf("next handler output missing fields: %q", buf.String())
	}
}

func TestHandler_NilBusIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	h := logbus.NewHandler(slog.NewTextHandler(&buf, nil), nil)
	log := slog.New(h)
	// Must not panic.
	log.Info("hi")
	if !strings.Contains(buf.String(), "msg=hi") {
		t.Fatalf("next still expected: %q", buf.String())
	}
}

func TestHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	bus := logbus.New()
	h := logbus.NewHandler(slog.NewTextHandler(&buf, nil), bus)
	log := slog.New(h).With("id", testInstance)

	sub, cancel := bus.Subscribe()
	defer cancel()
	log.Info("ev", "extra", "x")

	r := mustReceive(t, sub)
	if r.Attrs["id"] != testInstance {
		t.Fatalf("WithAttrs id missing: %+v", r.Attrs)
	}
	if r.Attrs["extra"] != "x" {
		t.Fatalf("inline attr missing: %+v", r.Attrs)
	}
}

func TestHandler_WithGroup(t *testing.T) {
	bus := logbus.New()
	h := logbus.NewHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), bus)
	log := slog.New(h).WithGroup("worker")

	sub, cancel := bus.Subscribe()
	defer cancel()
	log.Info("ev", "id", testInstance, "state", "idle")

	r := mustReceive(t, sub)
	if r.Attrs["worker.id"] != testInstance {
		t.Fatalf("group prefix missing on id: %+v", r.Attrs)
	}
	if r.Attrs["worker.state"] != "idle" {
		t.Fatalf("group prefix missing on state: %+v", r.Attrs)
	}
}

func TestHandler_GroupAttr(t *testing.T) {
	bus := logbus.New()
	h := logbus.NewHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), bus)
	log := slog.New(h)

	sub, cancel := bus.Subscribe()
	defer cancel()
	log.Info("ev", slog.Group("billing", "model", "hourly", "margin", "5m"))

	r := mustReceive(t, sub)
	if r.Attrs["billing.model"] != "hourly" {
		t.Fatalf("group attr model missing: %+v", r.Attrs)
	}
	if r.Attrs["billing.margin"] != "5m" {
		t.Fatalf("group attr margin missing: %+v", r.Attrs)
	}
}

func TestHandler_EnabledDelegates(t *testing.T) {
	// next is INFO+; DEBUG should be disabled.
	next := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := logbus.NewHandler(next, nil)
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("DEBUG should be disabled")
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("INFO should be enabled")
	}
}

func TestHandler_LevelFlattening(t *testing.T) {
	bus := logbus.New()
	h := logbus.NewHandler(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}), bus)
	log := slog.New(h)
	sub, cancel := bus.Subscribe()
	defer cancel()

	log.Warn("oops")
	r := mustReceive(t, sub)
	if r.Level != slog.LevelWarn {
		t.Fatalf("warn level: got %v", r.Level)
	}
	if r.Level.String() != "WARN" {
		t.Fatalf("warn String: got %q", r.Level.String())
	}
}

func mustReceive(t *testing.T, ch <-chan logbus.Record) logbus.Record {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for record")
		return logbus.Record{}
	}
}

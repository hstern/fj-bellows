package orchestrator

import (
	"testing"
	"time"
)

func TestPoolPutGetDelete(t *testing.T) {
	p := NewPool()
	if _, ok := p.Get("x"); ok {
		t.Fatal("empty pool returned a node")
	}
	p.Put(&Node{InstanceID: "x", State: StateProvisioning})
	got, ok := p.Get("x")
	if !ok || got.State != StateProvisioning {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if p.Len() != 1 {
		t.Errorf("Len = %d", p.Len())
	}
	p.Delete("x")
	if p.Len() != 0 {
		t.Errorf("Len after delete = %d", p.Len())
	}
}

func TestPoolPutCopies(t *testing.T) {
	p := NewPool()
	n := &Node{InstanceID: "x", State: StateIdle}
	p.Put(n)
	n.State = StateBusy // must not affect the stored copy
	got, _ := p.Get("x")
	if got.State != StateIdle {
		t.Errorf("stored node mutated externally: %s", got.State)
	}
}

func TestPoolSetStateAndByState(t *testing.T) {
	p := NewPool()
	if p.SetState("missing", StateIdle) {
		t.Error("SetState on missing node returned true")
	}
	p.Put(&Node{InstanceID: "a", State: StateIdle})
	p.Put(&Node{InstanceID: "b", State: StateIdle})
	p.Put(&Node{InstanceID: "c", State: StateBusy})
	if !p.SetState("a", StateBusy) {
		t.Error("SetState failed")
	}
	idle := p.ByState(StateIdle)
	if len(idle) != 1 || idle[0].InstanceID != "b" {
		t.Errorf("ByState(idle) = %+v", idle)
	}
}

func TestPoolTouch(t *testing.T) {
	p := NewPool()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.Put(&Node{InstanceID: "a"})
	p.Touch("a", ts)
	got, _ := p.Get("a")
	if !got.LastBusy.Equal(ts) {
		t.Errorf("LastBusy = %s", got.LastBusy)
	}
}

func TestPoolIDs(t *testing.T) {
	p := NewPool()
	p.Put(&Node{InstanceID: "a"})
	p.Put(&Node{InstanceID: "b"})
	ids := p.IDs()
	if _, ok := ids["a"]; !ok {
		t.Error("missing a")
	}
	if _, ok := ids["b"]; !ok {
		t.Error("missing b")
	}
	if len(ids) != 2 {
		t.Errorf("IDs len = %d", len(ids))
	}
}

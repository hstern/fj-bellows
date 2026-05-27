package orchestrator

import (
	"sync"
	"time"
)

// NodeState is a worker VM's lifecycle state.
type NodeState string

const (
	// StateProvisioning means the VM is created and awaiting SSH readiness.
	StateProvisioning NodeState = "provisioning"
	// StateIdle means the node is ready and warm with no job assigned.
	StateIdle NodeState = "idle"
	// StateBusy means a one-job run is in flight.
	StateBusy NodeState = "busy"
	// StateDraining means the node is marked for teardown, DELETE not yet issued.
	StateDraining NodeState = "draining"
	// StateRemoving means Destroy was issued, awaiting disappearance from List.
	StateRemoving NodeState = "removing"
)

// Node is the orchestrator's view of a worker VM.
type Node struct {
	InstanceID string
	State      NodeState
	// IP is the worker's public IPv4 (the legacy dial address under
	// transport.mode=ssh). Empty for providers that dispatch by container
	// exec, and may be empty under future private-only configurations.
	IP string
	// VPCIP is the worker's IPv4 on the provider VPC, when one is
	// configured. Empty when no VPC is in use. Under transport.mode=
	// cache-gateway (FJB-54) this is the address the orchestrator dials
	// for dispatch; under transport.mode=ssh it's informational only.
	VPCIP string
	// CreatedAt is the provider's creation time; it anchors the billing-hour
	// timer and is rebuilt from List on restart.
	CreatedAt time.Time
	// LastBusy is when the node last finished (or started) a job; it drives the
	// per-second idle timeout.
	LastBusy time.Time
	// CurrentJob is the Forgejo job handle in flight on this node. The
	// dispatch goroutine sets it on Busy and clears it on the Idle return.
	// Empty unless State == StateBusy.
	CurrentJob string
}

// Pool is the concurrency-safe set of nodes. The reconcile loop is the only
// writer of provisioning decisions; dispatch/teardown goroutines mutate only
// their own node's state through these methods.
type Pool struct {
	mu    sync.Mutex
	nodes map[string]*Node
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{nodes: map[string]*Node{}}
}

// Put inserts or replaces a node.
func (p *Pool) Put(n *Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := *n
	p.nodes[n.InstanceID] = &cp
}

// Delete removes a node.
func (p *Pool) Delete(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.nodes, id)
}

// Get returns a copy of a node and whether it exists.
func (p *Pool) Get(id string) (Node, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// SetState transitions a node if it exists, returning whether it did.
func (p *Pool) SetState(id string, s NodeState) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return false
	}
	n.State = s
	return true
}

// Touch records that a node just finished a job (now Idle).
func (p *Pool) Touch(id string, t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.nodes[id]; ok {
		n.LastBusy = t
	}
}

// SetJob records the Forgejo job handle in flight on a node. Pass "" to
// clear when the dispatch goroutine returns the node to Idle.
func (p *Pool) SetJob(id, handle string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.nodes[id]; ok {
		n.CurrentJob = handle
	}
}

// Snapshot returns copies of all nodes.
func (p *Pool) Snapshot() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, *n)
	}
	return out
}

// IDs returns the set of known instance IDs.
func (p *Pool) IDs() map[string]struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]struct{}, len(p.nodes))
	for id := range p.nodes {
		out[id] = struct{}{}
	}
	return out
}

// ByState returns copies of nodes in the given state.
func (p *Pool) ByState(s NodeState) []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Node
	for _, n := range p.nodes {
		if n.State == s {
			out = append(out, *n)
		}
	}
	return out
}

// Len returns the number of nodes in the pool.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.nodes)
}

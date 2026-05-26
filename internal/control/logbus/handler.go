package logbus

import (
	"context"
	"log/slog"
)

// Handler is a slog.Handler that delegates every record to a wrapped
// "next" handler (for the daemon's normal stderr text output) AND publishes
// a Record copy to a Bus so StreamLogs subscribers receive the same record.
//
// WithAttrs/WithGroup return a new Handler that carries the accumulated
// attrs and group prefix; both are flattened into Record.Attrs when a
// record is published.
type Handler struct {
	next   slog.Handler
	bus    *Bus
	attrs  []slog.Attr // pre-applied attrs (from WithAttrs)
	groups []string    // pre-applied group prefix (from WithGroup)
}

// NewHandler wraps next so every record it Handle()s is also published to
// bus. next must be non-nil; bus may be nil (then publish becomes a no-op,
// useful in tests).
func NewHandler(next slog.Handler, bus *Bus) *Handler {
	return &Handler{next: next, bus: bus}
}

// Enabled delegates to the wrapped handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle publishes a Record to the bus then forwards to the wrapped handler.
// The publish always happens first so a slow next handler (e.g. a stuck
// writer) can't keep operators tailing the control plane from seeing a
// fresh record.
func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if h.bus != nil {
		h.bus.Publish(h.toRecord(record))
	}
	return h.next.Handle(ctx, record)
}

// WithAttrs returns a child handler that includes attrs on every record.
// Pre-applied attrs are flattened into Record.Attrs at publish time and
// forwarded to the wrapped handler unchanged.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := *h
	cp.attrs = make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	cp.attrs = append(cp.attrs, h.attrs...)
	cp.attrs = append(cp.attrs, attrs...)
	cp.next = h.next.WithAttrs(attrs)
	return &cp
}

// WithGroup returns a child handler that prefixes every attr key with name.
// Empty name is a no-op per the slog.Handler contract.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	cp.groups = append([]string(nil), h.groups...)
	cp.groups = append(cp.groups, name)
	cp.next = h.next.WithGroup(name)
	// Pre-applied attrs are now logically inside the new group, but slog's
	// semantics put WithAttrs-then-WithGroup attrs at the parent level.
	// We've already forwarded those attrs into the next handler at
	// WithAttrs time, so we only need to track new attrs added in the
	// child's group context. Clear the pre-applied list here so they
	// aren't double-prefixed by groupPrefix at publish time.
	cp.attrs = nil
	return &cp
}

// toRecord builds a logbus.Record from a slog.Record, flattening attrs and
// applying any pre-applied attrs from WithAttrs and group prefix from
// WithGroup.
func (h *Handler) toRecord(record slog.Record) Record {
	attrs := map[string]string{}
	// Pre-applied attrs sit at the parent level (no group prefix).
	for _, a := range h.attrs {
		flattenAttr("", a, attrs)
	}
	// In-record attrs sit inside the current group prefix.
	prefix := groupPrefix(h.groups)
	record.Attrs(func(a slog.Attr) bool {
		flattenAttr(prefix, a, attrs)
		return true
	})
	return Record{
		At:      record.Time,
		Level:   record.Level,
		Message: record.Message,
		Attrs:   attrs,
	}
}

// groupPrefix joins groups with "." and appends a trailing "." so that
// callers can prepend it to an attr key directly.
func groupPrefix(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	n := 0
	for _, g := range groups {
		n += len(g) + 1
	}
	out := make([]byte, 0, n)
	for i, g := range groups {
		if i > 0 {
			out = append(out, '.')
		}
		out = append(out, g...)
	}
	out = append(out, '.')
	return string(out)
}

// flattenAttr writes a slog.Attr into out under prefix. Group-valued attrs
// recurse with prefix+name+".". Non-string scalars stringify via
// slog.Value.String().
func flattenAttr(prefix string, a slog.Attr, out map[string]string) {
	// Resolve LogValuer/Any indirection.
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		groupName := a.Key
		var childPrefix string
		switch {
		case prefix == "" && groupName == "":
			childPrefix = ""
		case groupName == "":
			childPrefix = prefix
		case prefix == "":
			childPrefix = groupName + "."
		default:
			childPrefix = prefix + groupName + "."
		}
		for _, ga := range v.Group() {
			flattenAttr(childPrefix, ga, out)
		}
		return
	}
	key := prefix + a.Key
	if v.Kind() == slog.KindString {
		out[key] = v.String()
		return
	}
	out[key] = v.String()
}

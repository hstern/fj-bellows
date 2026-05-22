package orchestrator

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under goroutine-leak detection.
// The orchestrator dispatches, provisions, and tears down in goroutines, so a
// missing exit path would show up here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

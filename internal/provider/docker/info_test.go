package docker

import (
	"context"
	"testing"
	"time"
)

// TestInfo covers the docker Info shape: every documented key is
// present and reflects the resolved config the dispatcher already uses.
func TestInfo(t *testing.T) {
	d := &Docker{
		cfg: config{
			DockerBin:   "/usr/bin/docker",
			Image:       "fj-bellows/worker:test",
			Network:     "host",
			WaitTimeout: Duration(30 * time.Second),
		},
	}
	info := d.Info(context.Background())

	cases := map[string]string{
		"docker_bin":   "/usr/bin/docker",
		"image":        "fj-bellows/worker:test",
		"network":      "host",
		"wait_timeout": "30s",
	}
	for k, want := range cases {
		if got := info[k]; got != want {
			t.Errorf("info[%q]: want %q got %q", k, want, got)
		}
	}
}

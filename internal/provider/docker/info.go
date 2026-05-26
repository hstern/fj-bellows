package docker

import "context"

// Info exposes operator-debug info via the control plane's ProviderInfo
// RPC. The local docker provider has very little state — the daemon
// socket path is the only thing worth surfacing. Implementing the
// optional InfoProvider interface (even trivially) keeps the wire
// response shape predictable: every provider answers, none make the
// operator wonder whether they typed the slug wrong.
//
// Keys:
//
//	docker_bin    — the resolved docker binary (defaulted in Configure)
//	image         — the worker image from provider_config
//	network       — the optional --network arg, empty when none
//	wait_timeout  — Go-duration string the dispatcher polls with
func (d *Docker) Info(_ context.Context) map[string]string {
	return map[string]string{
		"docker_bin":   d.cfg.DockerBin,
		"image":        d.cfg.Image,
		"network":      d.cfg.Network,
		"wait_timeout": d.cfg.WaitTimeout.D().String(),
	}
}

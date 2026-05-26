package linode

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Info exposes operator-debug info via the control plane. Keys (all values
// are strings — Prom-style, jq-friendly):
//
//	region                     — configured region
//	type                       — configured instance type
//	image                      — configured Linode image ID
//	firewall_id                — managed firewall ID, or 0 if not managed
//	placement_group_id         — managed PG ID, or 0
//	vpc_id                     — managed VPC ID, or 0
//	cache_linode_id            — managed cache VM ID, or 0
//	workers_in_flight          — count of in-flight Provision calls (pending)
//	capacity_full_count_24h    — count of "capacity full" 400s in the last 24h
//	account_balance_usd        — operator account balance from /account, or
//	                             empty if the PAT can't read it
//
// The single Linode API call is GET /account; it is gated by the caller's
// ctx (the handler thread). Everything else reads in-memory state. This
// must not include secrets — account_balance_usd is a number we already
// pull on every billing cycle from Linode's side; the rest are resource
// IDs that already appear in the deployment's audit log.
func (l *Linode) Info(ctx context.Context) map[string]string {
	out := map[string]string{
		"region":                  l.cfg.Region,
		"type":                    l.cfg.Type,
		"image":                   l.cfg.Image,
		"firewall_id":             strconv.Itoa(l.firewallIDForInfo()),
		"placement_group_id":      strconv.Itoa(l.placementGroupIDForInfo()),
		"vpc_id":                  strconv.Itoa(l.vpcIDForInfo()),
		"cache_linode_id":         strconv.Itoa(l.cacheLinodeIDForInfo()),
		"workers_in_flight":       strconv.FormatInt(l.workersInFlight.Load(), 10),
		"capacity_full_count_24h": strconv.Itoa(l.capacityFull.count(time.Now())),
		"account_balance_usd":     l.accountBalanceForInfo(ctx),
	}
	return out
}

// firewallIDForInfo prefers the managed firewall's id when present,
// falling back to the attach-by-id config field, and finally 0 (no
// firewall configured).
func (l *Linode) firewallIDForInfo() int {
	switch {
	case l.fw != nil:
		return l.fw.id
	case l.cfg.FirewallID != 0:
		return l.cfg.FirewallID
	}
	return 0
}

// placementGroupIDForInfo mirrors firewallIDForInfo: managed PG > attach-
// by-id > 0.
func (l *Linode) placementGroupIDForInfo() int {
	switch {
	case l.pg != nil:
		return l.pg.id
	case l.cfg.PlacementGroupID != 0:
		return l.cfg.PlacementGroupID
	}
	return 0
}

// vpcIDForInfo returns the managed VPC's id, or 0 if no managed VPC is
// configured. Linode does not yet support an attach-by-id VPC mode.
func (l *Linode) vpcIDForInfo() int {
	if l.vpc == nil {
		return 0
	}
	return l.vpc.id
}

// cacheLinodeIDForInfo returns the managed cache VM's Linode ID, or 0
// if no cache: block is configured or the cache hasn't been ensured yet.
func (l *Linode) cacheLinodeIDForInfo() int {
	if l.cache == nil {
		return 0
	}
	return l.cache.linodeID
}

// accountBalanceForInfo does a one-shot GET /account and formats the
// balance as e.g. "12.34". Returns "" when the PAT can't read /account
// (4xx — typically "Account: Read/Write" scope missing on a worker-
// scoped token) so the operator sees an empty value rather than a
// misleading 0.00. ctx bounds the call; on success or any error the
// result is computed once per Info() invocation.
func (l *Linode) accountBalanceForInfo(ctx context.Context) string {
	acct, err := l.client.GetAccount(ctx)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%.2f", acct.Balance)
}

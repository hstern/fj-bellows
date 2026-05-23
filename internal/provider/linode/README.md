# internal/provider/linode

The Linode implementation of `provider.Provider`, built on
[`linodego`](https://github.com/linode/linodego).

`provider_config` shape:

```yaml
provider_config:
  region: <linode-region>
  type:   <linode-instance-type>
  image:  <linode-image>
  token:  <provider-api-token>

  # Managed Cloud Firewall (recommended). Mutually exclusive with firewall_id.
  firewall:
    allow_inbound:
      - 203.0.113.5/32
      - auto              # host's external IPv4 (/32) + IPv6 (/128)
    refresh_interval: 1h  # optional; default 1h, minimum 1m

  # Alternative: attach to an operator-managed firewall by ID. Use this when
  # you'd rather manage the rules yourself. Mutually exclusive with `firewall`.
  # firewall_id: 12345
```

### Managed firewall (`firewall:` block)

When the `firewall` block is set, fj-bellows creates **one Cloud Firewall per
deployment** (keyed by `cfg.Tag`), attaches every provisioned worker to it,
and cleans it up when the last worker is destroyed. The firewall has a
default-deny inbound posture: only tcp/22 from the resolved `allow_inbound`
CIDRs. Outbound is unrestricted (workers need HTTPS to Forgejo, registries,
etc.).

`allow_inbound` accepts literal CIDRs plus one sentinel:

| Token | Expands to |
|---|---|
| `<cidr>` | itself |
| `auto` | host's external IPv4 (`/32`) and IPv6 (`/128`), via icanhazip |

Sentinel resolution is **fatal at Configure** (startup) — a sentinel that
can't resolve or an `allow_inbound` that ends up empty makes the daemon
refuse to start, rather than silently provisioning workers nobody can
reach. The refresh goroutine handles drift after that: every
`refresh_interval` it re-resolves the sentinels and updates the firewall
rules if they changed. Runtime refresh failure is non-fatal — the
previous-known-good ruleset stays in place; the daemon stays usable.

Stateful conntrack on Linode firewalls means in-flight SSH sessions from
the old IP aren't killed by a rule swap; only future connections are
gated.

**PAT scope** for managed mode: `Linodes: Read/Write` **and** `Firewalls:
Read/Write`. The simpler `firewall_id` mode below only needs `Linodes:
Read/Write`.

**Label / tag**: the firewall is labelled `fj-bellows-<sanitize(cfg.Tag)>`
(truncated to Linode's 32-char cap with a SHA-256 suffix when needed) and
tagged with `cfg.Tag`. Lookup uses the tag — labels are for human
inspection.

**IP-literal Forgejo URLs** (e.g. `https://192.0.2.10/`): the
hostname-override piece in the dispatcher doesn't apply (see #37); this is
unrelated to the firewall mode and not a managed-firewall limitation.

### `firewall_id` — attach to an operator-managed firewall

When `firewall_id` is set to a non-zero integer, every Linode created by
this provider is attached to that Cloud Firewall at create time
(`InstanceCreateOptions.FirewallID`). The firewall itself — its rules and
lifecycle — is operator-managed out of band; fj-bellows only attaches to it.

PAT scope: attaching an existing firewall via `firewall_id` on instance
creation only requires the standard `Linodes: Read/Write` scope. The Linode
API treats `firewall_id` as an attachment-by-reference parameter on
`POST /linode/instances` and does not require any `Firewalls` scope. We do
not list, create, or modify firewalls in this mode.

- **Provision** — `CreateInstance` with the rendered cloud-init passed as
  base64 user-data via the Linode Metadata service, the orchestrator's public
  key injected, and the pool tag stamped. Returns the instance with the
  provider-reported `CreatedAt` (which anchors the billing-hour timer).
- **Destroy** — `DeleteInstance`.
- **List(tag)** — lists instances and filters by tag.
- **BillingModel** — `BillingHourlyRoundUp` (Linode bills whole hours rounded
  up), so the core warm-holds and applies the `:55` teardown rule.

cloud-init is provider-agnostic and rendered by `internal/bootstrap`; this
package only forwards it as user-data.

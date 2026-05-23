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

    # Optional. Override the defaults (DROP / ACCEPT). DROP outbound forces
    # an explicit allow list — workers need HTTPS to Forgejo, registries,
    # and apt/yum, so this typically requires extra_outbound rules below to
    # avoid breaking runner boot.
    # inbound_policy: DROP
    # outbound_policy: ACCEPT

    # Optional. Extra rules appended after the synthesized tcp/22 rules
    # (inbound) or used as the entire outbound list. Each rule mirrors the
    # Linode FirewallRule shape; addresses are already-shaped CIDRs.
    # extra_inbound:
    #   - label: prometheus
    #     ports: "9100"
    #     protocol: TCP
    #     action: ACCEPT
    #     addresses:
    #       ipv4: [10.0.0.0/8]
    # extra_outbound:
    #   - label: deny-smtp
    #     ports: "25"
    #     protocol: TCP
    #     action: DROP
    #     addresses: [any]   # 0.0.0.0/0 + ::/0; see sentinel table below

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

`allow_inbound` (and the `addresses:` field on `extra_inbound` /
`extra_outbound` rules) accept literal CIDRs plus a unified set of sentinels:

| Token | Expands to |
|---|---|
| `<cidr>` | itself |
| `auto` | host's external IPv4 (`/32`) and IPv6 (`/128`), via icanhazip |
| `any` | `0.0.0.0/0` + `::/0` |
| `any-v4` | `0.0.0.0/0` |
| `any-v6` | `::/0` |

The sentinel vocabulary is the same across both surfaces — `auto` lets an
extra rule track the operator's IP (e.g. an SSH allow-from-here rule on a
non-standard port), and `any[-vN]` is the natural shorthand for a
permissive rule (e.g. an `extra_outbound` DROP-from-anywhere block).

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

#### Policy overrides + extra rules (`inbound_policy`, `outbound_policy`, `extra_inbound`, `extra_outbound`)

The defaults — `inbound_policy: DROP`, `outbound_policy: ACCEPT` — are right
for the common case (workers accept only inbound tcp/22 from `allow_inbound`,
and need unrestricted outbound for HTTPS to Forgejo, registries, package
mirrors). The optional knobs cover the cases the defaults don't fit:

- **`inbound_policy: ACCEPT`** flips to default-allow on inbound. Don't use
  this unless `extra_inbound` lists every port you want to block — workers
  with default-allow inbound and only your synthesized SSH rule are
  effectively wide open. A misuse this way is a bigger footgun than the
  default.
- **`outbound_policy: DROP`** forces an explicit egress allow list. Workers
  must still reach Forgejo (over the dispatcher's reverse-tunnel; see #33),
  the runner-version download (`code.forgejo.org`), and whatever package /
  image registries the workflow uses (Docker Hub, GHCR, apt/yum mirrors).
  Set this only with `extra_outbound` rules listing every required egress —
  an over-tight outbound list stops the worker from booting.
- **`extra_inbound` / `extra_outbound`** add rules verbatim in the linodego
  shape. Each rule's `addresses.ipv4` and `addresses.ipv6` are capped at 255
  entries (the Linode per-rule limit); the combined inbound rule count
  (synth + extras) must stay under the 25-rule-per-firewall ceiling, same
  for outbound on its own.

The refresh goroutine only re-resolves the `allow_inbound` sentinels; the
operator's extras + policies are read once at Configure and never reloaded
on the running daemon. Restart to pick up edits to those fields.

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

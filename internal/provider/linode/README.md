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

  # Managed Linode Placement Group (anti-affinity:local). Mutually exclusive
  # with placement_group_id below.
  placement_group:
    enforcement: flexible   # or strict — see below

  # Alternative: attach every worker to a pre-existing PG by ID. Mutually
  # exclusive with `placement_group` above.
  # placement_group_id: 67890
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

### Managed placement group (`placement_group:` block)

When the `placement_group` block is set, fj-bellows creates **one Linode
Placement Group per deployment** (keyed by `cfg.Tag`'s label), attaches
every provisioned worker to it, and reaps the group when the last worker
is destroyed. The affinity type is `anti_affinity:local` (the only value
Linode accepts today) — workers spread across distinct hosts within the
region, so a single hardware failure doesn't take out the whole pool.

```yaml
provider_config:
  # ...
  placement_group:
    # Optional. flexible (default) lets Linode fall back to colocation
    # when no compliant slot is available; strict refuses the Provision
    # instead.
    enforcement: flexible
```

**Enforcement**:

- **`flexible`** (default): when Linode can't satisfy the anti-affinity
  constraint (host pool exhausted, etc.), it places the Linode on a
  shared host anyway. Best-effort isolation; never blocks a worker boot.
- **`strict`**: refuses the create when the anti-affinity slot isn't
  available. Provision returns an error; the orchestrator surfaces it
  and retries next tick. Use when the isolation guarantee matters more
  than worker availability.

**PAT scope** for managed mode: `Linodes: Read/Write` **and** `Placement
Groups: Read/Write`. The simpler `placement_group_id` mode below only
needs `Linodes: Read/Write`.

**Label**: `fj-bellows-<sanitize(cfg.Tag)>`, truncated to Linode's 64-char
PG label cap with a SHA-256 suffix when needed. Unlike Cloud Firewalls,
placement groups have no `tags` field — ownership matching is by label
alone. Two deployments with distinct `cfg.Tag` get distinct labels →
distinct groups.

**Lifecycle**:

- Eager create at Configure (mirror of managed firewall — PAT-scope
  mistakes surface at startup).
- No refresh goroutine; PG policy + label can't drift the way an
  external IP can.
- Last `Destroy` in a deployment triggers `maybeCleanupPlacementGroup`
  via the same per-instance Destroy path that reaps the firewall. The
  PG is deleted once `GetPlacementGroup.Members` is empty (Linode
  auto-unassigns destroyed instances from their group).

### `placement_group_id` — attach to an operator-managed PG

Mutually exclusive with `placement_group`. Mirrors `firewall_id`: every
provisioned worker is attached to the named group at create time, and
fj-bellows does nothing else. No extra PAT scope.

The group's region must match `region:` above (Linode rejects the
attach otherwise).

## VPC

Optional. Stands up a Linode VPC for the deployment and attaches every
provisioned worker to a configured subnet (in addition to its public NIC).
Useful on its own for worker-to-worker private comms; designed to also
host the pull-through registry cache (FJB-6) so the cache binds to a VPC
IP and is unreachable from the public internet.

### `vpc:` — managed mode

```yaml
provider_config:
  region: us-ord
  # ...
  vpc:
    # At least one subnet required. CIDRs must be RFC1918 (10/8,
    # 172.16/12, 192.168/16) — Linode rejects CGNAT (100.64.0.0/10)
    # and public ranges with `400 [subnets[N].ipv4] The subnet ... is
    # not in the allowed VPC ranges`. CIDRs don't escape the VPC, so
    # any vacant RFC1918 range works — pick one that doesn't overlap
    # your LAN if you ever VPN/peer.
    subnets:
      cache:
        ipv4: 10.0.0.0/24
    # Optional. Defaults to the alphabetically-first subnet key.
    # worker_subnet: cache
```

When `vpc:` is set, fj-bellows creates **one VPC per deployment** (keyed
by `cfg.Tag` via the VPC's label) with the configured subnets, attaches
every worker's second NIC to the resolved `worker_subnet`, and reaps the
VPC plus its subnets when the last worker is destroyed.

**Worker attachment**: workers get an explicit two-NIC config —
`{public, primary} + {vpc, subnet_id}`. The public NIC stays primary so
default-route egress is unchanged (workers still pull from upstream
registries and package mirrors over the public network). Removing the
public NIC would require an out-of-VPC NAT, which is out of scope.

**Subnet naming**: subnets are keyed by name in the YAML; the Linode
label is `fj-bellows-<sanitize(cfg.Tag + "-" + name)>`. Two deployments
using the same subnet name (e.g. both `cache`) don't collide because
`cfg.Tag` is in the label.

**PAT scope**: managed VPC adds `VPCs: Read/Write` on top of the existing
`Linodes: Read/Write`. (`firewall:` adds `Firewalls: Read/Write`,
`placement_group:` adds `Placement Groups: Read/Write` — they stack.)

**Labels**: VPC labels are restricted to `[A-Za-z0-9-]` (no underscores
or dots, unlike firewalls and PGs). The sanitizer replaces disallowed
chars with `-` and truncates over-long labels with a SHA-256-derived
suffix so two distinct long tags don't collide. Override the
auto-derived label with `vpc.name: <label>` when running multiple
deployments that need distinct human-readable VPC names.

**Lifecycle**:

- Eager create at Configure — same rationale as firewall + PG (PAT-scope
  mistakes surface at startup, not on the first job arrival).
- Adopt-existing on restart: if a VPC with the matching label already
  exists in the region, fj-bellows reuses it and creates any subnets
  declared in config that aren't yet present on it.
- Last `Destroy` in a deployment triggers `maybeCleanupVPC` via the same
  per-instance Destroy path that reaps the firewall + PG. The VPC and
  all its subnets are deleted once every subnet's `Linodes` field is
  empty — Linode reports per-subnet attachment counts on `GetVPC`, so
  cleanup is graph-aware and won't race a still-attached worker.

### Operator-managed VPC mode

Not yet supported. The shape is non-trivial (operator has to supply both
the VPC ID and the per-worker subnet ID, and there's no obvious "managed
subnets within an operator-managed VPC" story to factor in). Would
mirror `firewall_id:` / `placement_group_id:` as `vpc_id:` +
`vpc_subnet_id:` if added later.

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

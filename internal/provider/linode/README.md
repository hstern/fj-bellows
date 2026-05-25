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
  fjb extends this semantic to **PG capacity** too (FJB-11): if the
  group is full (`scale.max` ≥ region's PG max-members) and the create
  call returns "Placement Group is at its full capacity", Provision
  drops the PG attach for that one Linode and retries. The PG fills
  back up at its own pace; this single worker just doesn't get the
  anti-affinity slot. A `WARN` log records the bypass.
- **`strict`**: refuses the create when the anti-affinity slot isn't
  available OR the PG is at capacity. Provision returns an error; the
  orchestrator surfaces it and retries next tick. Use when the
  isolation guarantee matters more than worker availability.

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

## Cache (FJB-6 PR 2a + PR 2b)

Optional. Stands up a [zot](https://zotregistry.dev/) registry as a
pull-through cache backed by Linode Object Storage, with workers
wired to pull through it.

### `cache:` — managed mode

```yaml
provider_config:
  region: us-ord
  # ... firewall: / placement_group: as before ...
  cache:
    # Required. Workers' containerd mirror config keys on the host of
    # this URL; zot's sync extension pull-throughs from this URL.
    upstream:
      url: https://forgejo.example.com/v2/
    # Optional. CA persistence path (load-or-generate) — daemon
    # restart with the same CA dir lets fj-bellows adopt the existing
    # cache VM instead of recreating it. Default: under
    # os.UserConfigDir() namespaced by the deployment tag.
    # tls:
    #   ca_dir: /var/lib/fj-bellows/<tag>/cache-ca
    # Optional VM tuning (all default):
    # type: g6-nanode-1            # bump to g6-standard-1 under burst pulls
    # image: linode/debian13
    # zot_version: 2.1.7           # pinned; bump deliberately
```

When `cache:` is set, fj-bellows:

1. **Auto-synthesizes the `vpc:` block** with a single `cache` subnet at
   `10.0.0.0/24` if you didn't declare one. Workers gain a VPC NIC on
   the same subnet (see VPC section above). Declare `vpc:` explicitly
   to override the CIDR or add more subnets.
2. **Mints an Object Storage bucket** named `fjb-cache-<tag>` in the
   provider region.
3. **Creates a scoped Object Storage access key** limited to that bucket
   (read_write). The secret is exposed by Linode exactly once at create
   time and lives in the cache VM's user-data; the orchestrator never
   persists it.
4. **Provisions a separate Linode** (default Nanode) tagged
   `<deployment-tag>-cache` (NOT the deployment tag — the worker
   `List(tag)` lookup is exact-match, so the cache never surfaces as a
   worker the reconciler tries to dispatch to). Its cloud-init downloads
   the pinned zot binary, generates a self-signed TLS cert with openssl,
   writes `/etc/zot/config.json` pointing at the bucket, and starts the
   `zot.service` systemd unit on port 5000.
5. **Attaches the cache to the deployment firewall and VPC** — the
   firewall's default `allow_inbound: [auto]` covers SSH; tcp/5000 is
   reachable only over the VPC NIC (the public NIC's firewall drops
   non-SSH inbound).

The cache VM uses the SAME deployment firewall as workers — no separate
firewall block. Operators get SSH break-glass via the operator's IP
(per the `auto` sentinel); registry traffic stays off the public NIC.

### Worker image requirement (Docker >= 23)

The worker `image:` must ship a Docker version that supports the
**containerd image store** (`{"features": {"containerd-snapshotter":
true}}` in `daemon.json`) — Docker 23+. Without it, dockerd uses its
own image store and silently ignores `/etc/containerd/certs.d/`, so
every job-container pull bypasses the cache and goes direct to
upstream (zero objects in the cache bucket — FJB-9). `linode/debian13`
(trixie, docker.io 26.1.5) is the practical floor; `linode/debian12`
(bookworm, docker.io 20.10) is too old. The cache-extras cloud-init
writes `/etc/docker/daemon.json` and restarts docker, so the operator
doesn't manage the daemon config — but the binary has to be new
enough to understand the feature.

### PAT scope

`cache:` adds **Object Storage: R/W** on top of:
- `Linodes: R/W` (always required)
- `Firewalls: R/W` (when `firewall:` is set)
- `Placement Groups: R/W` (when `placement_group:` is set)
- `VPCs: R/W` (when `vpc:` is set OR auto-synthesized via `cache:`)

### Linode account prerequisites

Object Storage must be **enabled on the account** ($5/mo flat,
one-click at Cloud Manager → Object Storage → "Enable Object Storage").
Without it, `cache:` Configure fails at the first `CreateObjectStorage*`
call with a Linode 403 — the error surfaces immediately at startup
(eager-create), not on first job. A dedicated pre-probe with a
clearer error message lands in PR 2b.

### Lifecycle

- **Eager create at Configure** — same rationale as firewall + VPC.
  Order: firewall → PG → VPC (auto or explicit) → cache.
- **Adopt-existing on restart**: if a cache VM with the matching
  `<tag>-cache` tag already exists, fj-bellows adopts it and skips
  bucket + key creation entirely. The existing VM keeps its baked-in
  credentials and stays serving. Daemon restarts therefore don't churn
  the cache.
- **Cache VM lifecycle is decoupled from worker count** (FJB-12). The
  cache VM, bucket, and scoped key all outlive the worker fleet —
  idle teardown reaps workers but leaves the cache warm so the next
  job after a quiet period doesn't pay 3-5 min of cache boot, and so
  the deadlock window where Provision needed the VPC IP from a
  just-reaped cache VM is closed. Firewall and VPC reapers still
  fire correctly because both gate on "no devices/linodes attached"
  and the cache VM keeps them in use; the placement group (which the
  cache VM doesn't join) reaps cleanly when the last worker leaves.
  Operators wanting an explicit teardown delete the cache VM via the
  Linode console (one Linode + one bucket — the cache is still
  re-createable on the next Provision via `cache.ensure()` per
  FJB-10 if you wanted to start fresh). A future fjb flag could
  automate this if it becomes a common operation.
- **Stale-key reaping**: at Configure, fj-bellows lists Object Storage
  keys whose label matches the deployment pattern and reaps any
  left-over keys from prior daemon lifetimes (rationale: Linode reveals
  `secret_key` only once at create, so any prior-lifetime key is
  unusable to the new daemon).

### Worker integration (PR 2b)

When `cache:` is set, the linode provider wraps each worker's
cloud-init in a multipart-MIME message whose second part is a
deployment-specific fragment that:

- Trusts the **fjb-managed CA** — written to
  `/usr/local/share/ca-certificates/fjb-cache.crt` and registered via
  `update-ca-certificates`. The CA is generated once and persisted to
  `cache.tls.ca_dir` so daemon restarts adopt the existing cache VM
  (signed by the same CA).
- Adds an `/etc/hosts` entry: `<cache VPC IP> cache.fjb.internal`.
  No DNS dependency — the hosts lookup wins. The VPC IP is looked up
  from the cache linode's config interfaces on first Provision after
  Configure and cached for the daemon's lifetime.
- Installs a containerd `hosts.toml` for the configured upstream host
  with **pull-only capabilities** (`["pull", "resolve"]`, NOT
  `"push"`). This is the boundary that keeps
  `docker push <upstream-host>/foo` going direct to upstream over the
  public NIC — the cache is a target only for explicit pushes to the
  cache hostname. Without this, push traffic would be silently
  captured by the cache and never reach the upstream registry.

The provider-side multipart wrap keeps the `bootstrap` package and
the orchestrator unaware of provider-specific cache concerns — they
render the standard worker cloud-init, the linode provider's
Provision adds the cache fragment on top. cloud-init merges the two
parts (write_files and runcmd entries from both are concatenated).

When `cache:` is unset the wrap is skipped entirely and worker
cloud-init is byte-identical to a no-cache deployment.

### TLS and CA persistence

The fjb-managed CA persists to `cache.tls.ca_dir` across daemon
restarts. Operators running fjb as a system service should set this
to a stable path (e.g. `/var/lib/fj-bellows/<tag>/cache-ca/`); the
default falls back to `os.UserConfigDir()` which is `~/.config/...`
on Linux when running under a user account.

Three outcomes at Configure-time:

1. **CA dir empty, no cache VM**: generate fresh CA, persist it,
   create cache VM signed by it. Workers trust the new CA.
2. **CA dir populated, no cache VM**: reuse persisted CA, create
   cache VM signed by it. (Typical "daemon restart after manual
   cache VM teardown".)
3. **CA dir populated, cache VM exists**: adopt the cache VM. CA on
   disk matches the one that signed the VM's baked-in cert; workers
   keep trusting it. (Typical "daemon restart with intact state".)

A fourth case — **CA dir empty, cache VM exists** — is detected and
rejected with a clear error. The operator picks: restore the CA
backup, or destroy the cache VM and let the next start recreate it
under the fresh CA.

### Upstream reachability — orchestrator reverse-tunnel (FJB-7)

The cache VM lives on Linode and reaches the operator's upstream
registry through a **persistent `ssh -R` tunnel from the
orchestrator**, not directly. This is what makes a LAN-internal
upstream (`https://git.stern.ca/v2/` on `192.168.x.x`, split-horizon
DNS, etc.) usable without exposing it on the public internet.

How it composes:

1. The cache VM cloud-init pins `127.0.0.1 <upstream-host>` in
   `/etc/hosts` and writes
   `/etc/ssh/sshd_config.d/20-fj-bellows-tunnel.conf` to keep
   `AllowTcpForwarding yes` (a hardened base image setting it to
   `no` would silently break sync).
2. After Configure creates the VM (or adopts an existing one), a
   long-lived orchestrator goroutine dials the cache VM over SSH
   (TOFU host-key pinning, same shape as the dispatcher's worker
   pin), opens a remote port-forward listener on
   `127.0.0.1:<upstream-port>` on the cache VM, and bridges each
   accepted connection to `<upstream-host>:<upstream-port>` from the
   orchestrator's own network namespace.
3. zot's sync extension dials `https://<upstream-host>/...`, the
   `/etc/hosts` override sends it to loopback, the listener forwards
   it through SSH to the orchestrator, the orchestrator dials the
   real upstream over its LAN/public route, and bytes flow.

TLS SNI stays the original hostname, so a public-CA cert on the
upstream continues to validate end-to-end on the cache VM.

The goroutine reconnects with exponential backoff (1s → 30s) and
shuts down cleanly on cache cleanup (last Destroy) so the loop
doesn't churn against a destroyed VM. Lifecycle matches the
managed-firewall refresh loop: `context.Background`-rooted, no
Provider.Shutdown hook required.

**IP-literal upstreams** (`https://10.0.0.5/v2/`): the `/etc/hosts`
override is skipped — /etc/hosts maps names not IPs, so loopback
redirection can't be done that way. The tunnel still starts but zot
will dial the IP directly and fail for LAN-internal IP literals.
Use a hostname upstream when the LAN-internal route matters.

**Pre-conditions**: the SSH key the dispatcher uses
(`cfg.ssh.private_key_file`) is plumbed into the linode provider via
`SetSSHIdentity` from `cmd/fj-bellows`; its public half is injected
into the cache VM's `authorized_keys` via cloud-init so the dial is
accepted. Deployments without an SSH key (e.g. docker-only) never
reach this path; deployments with cache disabled never start the
tunnel.

### Operator-managed cache mode

Not supported and not planned. Cache is a multi-resource bundle (VM +
bucket + key + cloud-init); the operator-managed shape would have to
own all three and the lifecycle coordination is non-trivial. Use the
fully-managed mode or none.

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

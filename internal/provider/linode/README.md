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
  # Optional. ID of a pre-existing Linode Cloud Firewall to attach to every
  # provisioned worker. Omit or set to 0 to skip attachment.
  firewall_id: 0
```

### `firewall_id` — Cloud Firewall attachment

When `firewall_id` is set to a non-zero integer, every Linode created by this
provider is attached to that Cloud Firewall at create time
(`InstanceCreateOptions.FirewallID`). The firewall itself — its rules and
lifecycle — is operator-managed out of band; fj-bellows only attaches to it.

Typical use: a default-deny-by-default firewall that allows inbound SSH only
from the orchestrator's egress address(es), so newly booted workers are
unreachable from the internet before cloud-init finishes.

PAT scope: attaching an existing firewall via `firewall_id` on instance
creation only requires the standard `Linodes: Read/Write` scope (the same
scope already needed to create and destroy instances). The Linode API treats
`firewall_id` as an attachment-by-reference parameter on
`POST /linode/instances` and does not require any `Firewalls` scope. We do
not list, create, or modify firewalls, so no `Firewalls` scope is needed.

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

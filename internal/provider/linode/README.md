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
```

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

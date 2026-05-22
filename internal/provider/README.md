# internal/provider

The cloud-provider abstraction and an in-tree registry.

```go
type Provider interface {
    Configure(node yaml.Node) error
    Provision(ctx context.Context, spec Spec) (Instance, error)
    Destroy(ctx context.Context, id string) error
    List(ctx context.Context, tag string) ([]Instance, error)
    BillingModel() BillingModel
}
```

Providers register themselves by name (typically in `init`) via `Register`, and
the core selects one with `New(name)`. The core hands each provider the opaque
`provider_config` node to `Configure`; it never references provider-specific
fields.

`BillingModel` is what makes teardown correct across clouds:

- `BillingPerSecond` — plain idle timeout (AWS/GCP/Azure).
- `BillingHourlyRoundUp` — warm-hold + `:55` hour-boundary teardown (Linode,
  Hetzner, old AWS).

`List(tag)` must reflect the provider's ground truth — it powers reconcile and
the orphan sweep, so a crashed daemon can rebuild its view (and its billing-hour
timers, via `Instance.CreatedAt`) from it.

Subpackages: [`linode`](linode) (implementation), [`mock`](mock) (test double).

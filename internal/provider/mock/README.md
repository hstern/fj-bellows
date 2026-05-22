# internal/provider/mock

A hand-written, configurable mock of `provider.Provider` for unit tests.

Each method delegates to a function field (`ProvisionFn`, `DestroyFn`,
`ListFn`, `BillingModelFn`, `ConfigureFn`); unset fields return zero values, so
a test sets only what it exercises. Calls are recorded
(`ProvisionCalls`/`ProvisionCount`, `DestroyCalls`/`DestroyCount`, `ListCalls`)
and the mock is safe for concurrent use, which matters because the orchestrator
calls providers from goroutines.

```go
prov := &mock.Provider{
    ProvisionFn: func(_ context.Context, s provider.Spec) (provider.Instance, error) {
        return provider.Instance{ID: "100", IPv4: "10.0.0.1", CreatedAt: time.Now()}, nil
    },
}
```

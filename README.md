# platform

Tooling for the service registry POC: the `svcreg` renderer and generator, the
sandbox propagation library services link against, and a small git server for
local clusters.

| Repo | Role |
|---|---|
| [mcafeelabs/services](https://github.com/mcafeelabs/services) | The registry: layers, sandboxes, generated `rendered/` and `apps/` |
| **mcafeelabs/platform** (this repo) | `svcreg`, `pkg/sandbox`, `pkg/servicekit`, `gitserver` |
| [mcafeelabs/platform-chart](https://github.com/mcafeelabs/platform-chart) | The central Helm chart and its `capabilities.yaml` |
| [orders](https://github.com/mcafeelabs/orders), [payments](https://github.com/mcafeelabs/payments), [inventory](https://github.com/mcafeelabs/inventory), [identity](https://github.com/mcafeelabs/identity) | Demo services, each owning its `service.yaml` |

## How it fits together

```
service repo ──CI──► services/<svc>/service.yaml + schema   (PR, auto-merge)
                         │
services repo CI ── svcreg validate ── graph checks, schema per env
                 └─ svcreg generate ─► rendered/<env>/<svc>/values.yaml
                                       rendered/sbx/<name>/<svc>/values.yaml
                                       apps/<env>/        (sync waves = graph depth)
                                       apps/_sandboxes/   (namespace, routes)
                                       apps/_platform/    (ApplicationSets, Kargo)
                         │
Argo CD ◄── ApplicationSets ── central chart + rendered values
Kargo   ◄── Warehouses (image + rendered paths) ── Stages copy values to stage branches
```

## svcreg

```
svcreg validate                     # publish-time checks + render every env and sandbox
svcreg graph [--format dot|json]    # waves (longest path from a root)
svcreg dependents identity          # who deploys after identity
svcreg render --env dev [--service orders]
svcreg effective-config orders dev [--sandbox NAME] [--layers]
svcreg generate [--check]           # write or verify rendered/ and apps/
svcreg sandbox-expire               # delete sandbox files past their TTL
svcreg lint-service --capabilities chart/capabilities.yaml   # in a service repo
svcreg serve --addr :8080           # read-only HTTP query service
svcreg kv-sync --nats URL --index sandboxes.json             # NATS KV for sandbox routing
```

`generate` also takes `--git-url`, `--git-revision`, `--chart-url`,
`--chart-revision`, `--image-registry` and `--tools-image`, which override
`registry.yaml` for one run (the Kind e2e points Argo CD at an in-cluster git
server this way). Those outputs are never committed.

### Checks

At publish time (`registry.Validate`):

- every `consumes` and `deployAfter` target exists, and each named output is in
  that service's `provides`;
- the graph is acyclic, and a cycle is reported by name
  (`identity -> orders -> identity`);
- output names are unique per service, and every `kind` and `chartProfile` is
  in the chart's `capabilities.yaml`;
- each schema is valid JSON Schema (draft 2020-12 by default);
- sandboxes name an existing baseline, existing services, and a valid TTL.

At render time, for every environment and sandbox:

- layers merge with Helm semantics: maps deep-merge, scalars and lists replace,
  and `null` deletes a key. Only the top-level keys `config`, `deployment`,
  `image` and `platform` are allowed;
- `$output` must point at an output the service lists under `consumes` (its
  own outputs need no entry);
- `x-ref` in the schema restricts which reference kinds a field accepts. Add
  `literal` to the list to also allow a plain value, so a password marked
  `x-ref: [secret]` can never be committed as plain text;
- the merged config is validated against the schema. A reference counts as a
  string, so the field's other constraints (pattern, format) are skipped for
  it.

### References

| Written | Rendered as |
|---|---|
| `$output: payments/api` (deterministic kind) | literal, e.g. `http://payments.dev.svc.cluster.local` |
| `$output: orders/db#password` (dynamic kind) | `$kv: ssm://services/dev/orders/db#password` |
| `$secret: aws-sm://orders/payments-api-key` | ExternalSecret entry against the env's `aws-sm` store |
| `$kv: ssm://orders/flags/checkout-v2` | ExternalSecret entry against the env's `ssm` store |

The chart gets `configFile.template` (config.yaml with `{{ .refN | toJson }}`
placeholders) and `configFile.refs`, and renders one ExternalSecret that
resolves each reference from its own store. Secret values never pass through
git or the renderer.

In a sandbox, an output resolves to the sandbox's copy only when the sandbox
runs the providing service **and** the output is `sandbox: isolate`. Otherwise
it resolves to the baseline. Shared HTTP endpoints therefore stay on baseline
hostnames, and the waypoint routes tagged requests into the sandbox.

## pkg/sandbox

Services forward the W3C `baggage` header (`sandbox=<name>`) using the OTel
baggage propagator:

```go
http.Handle("/", sandbox.Middleware(mux))                   // extract
client := &http.Client{Transport: sandbox.Transport(nil)}   // inject
sandbox.Publish(ctx, js, "orders.created", data)            // NATS headers
sandbox.Consumer{Service: "orders", Sandbox: os.Getenv("SANDBOX_NAME"),
  Stream: "INVENTORY", Overrides: o}.Run(ctx, js, handler)
```

On NATS, the baseline consumer acks and skips messages for sandboxes that
override its service. A sandbox consumer handles only its own sandbox's
messages and uses its own durable name, `<svc>-sbx-<name>`, with
`InactiveThreshold`, so JetStream removes it after teardown. The overrides come
from the `sandboxes` KV bucket, which `svcreg kv-sync` keeps in step with
`sandboxes/*.yaml`.

## Development

```
go test ./...          # includes an embedded JetStream server for the NATS tests
go run ./cmd/svcreg validate --root ../services
```

Images: `ghcr.io/mcafeelabs/svcreg` and `ghcr.io/mcafeelabs/gitserver` (built on
pushes to `main` and on `v*` tags).

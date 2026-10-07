package registry_test

import (
	"strings"
	"testing"

	"github.com/mcafeelabs/platform/internal/testutil"
	"github.com/mcafeelabs/platform/pkg/registry"
)

func load(t *testing.T, root string) *registry.Registry {
	t.Helper()
	r, err := registry.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFixtureIsValid(t *testing.T) {
	r := load(t, testutil.Registry(t))
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.ServiceNames(), ","); got != "identity,inventory,orders,payments" {
		t.Fatalf("services %s", got)
	}
	if got := strings.Join(r.EnvNames(), ","); got != "dev,staging" {
		t.Fatalf("envs %s", got)
	}
	sb := r.Sandboxes["ryan-checkout-v2"]
	if sb == nil || !sb.Overrides("orders") || sb.Overrides("identity") {
		t.Fatalf("sandbox %+v", sb)
	}
}

func TestPublishTimeChecks(t *testing.T) {
	cases := map[string]struct {
		edit func(t *testing.T, root string)
		want []string
	}{
		"missing consume target": {
			edit: func(t *testing.T, root string) {
				testutil.Write(t, root, "services/payments/service.yaml", `
apiVersion: platform/v1
name: payments
owner: team-payments
chartProfile: http-api
provides: [{name: api, kind: http-endpoint}]
consumes:
  - {service: identity, output: grpc}
  - {service: ledger, output: api}
deployAfter: [{service: nowhere}]
config: {schema: ./config-values.schema.yaml}
`)
			},
			want: []string{
				`consumes identity/grpc: identity does not provide "grpc"`,
				`consumes ledger/api: service "ledger" does not exist`,
				`deployAfter nowhere: service does not exist`,
			},
		},
		"cycle": {
			edit: func(t *testing.T, root string) {
				testutil.Write(t, root, "services/identity/service.yaml", `
apiVersion: platform/v1
name: identity
owner: team-identity
chartProfile: http-api
provides: [{name: api, kind: http-endpoint}]
deployAfter: [{service: orders}]
config: {schema: ./config-values.schema.yaml}
`)
			},
			want: []string{"deployment graph has a cycle: identity -> orders -> identity"},
		},
		"unsupported kind, profile and duplicate output": {
			edit: func(t *testing.T, root string) {
				testutil.Write(t, root, "services/inventory/service.yaml", `
apiVersion: platform/v1
name: inventory
owner: team-fulfillment
chartProfile: lambda
provides:
  - {name: api, kind: http-endpoint}
  - {name: api, kind: dynamodb-table, sandbox: maybe}
config: {schema: ./config-values.schema.yaml}
`)
			},
			want: []string{
				`chartProfile "lambda" is not supported`,
				`output name "api" is declared twice`,
				`kind "dynamodb-table" is not supported`,
				`sandbox must be "share" or "isolate"`,
			},
		},
		"bad sandbox": {
			edit: func(t *testing.T, root string) {
				testutil.Write(t, root, "sandboxes/broken.yaml", `
name: broken
owner: someone
baseline: prod
ttl: forever
services: [{name: orders}, {name: orders}, {name: ghost}]
`)
			},
			want: []string{
				`baseline environment "prod" does not exist`,
				`ttl "forever"`,
				`service "orders" is listed twice`,
				`service "ghost" does not exist`,
			},
		},
		"layer for unknown environment": {
			edit: func(t *testing.T, root string) {
				testutil.Write(t, root, "services/orders/environments/qa/values.yaml", "config: {}\n")
			},
			want: []string{`environment "qa" does not exist`},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := testutil.Registry(t)
			c.edit(t, root)
			r, err := registry.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			err = r.Validate()
			if err == nil {
				t.Fatal("expected validation errors")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("missing %q in:\n%v", w, err)
				}
			}
		})
	}
}

func TestInvalidSchemaFailsLoad(t *testing.T) {
	root := testutil.Registry(t)
	testutil.Write(t, root, "services/identity/config-values.schema.yaml", "type: 12\n")
	if _, err := registry.Load(root); err == nil || !strings.Contains(err.Error(), "invalid JSON Schema") {
		t.Fatalf("got %v", err)
	}
}

func TestSandboxNameMustMatchFile(t *testing.T) {
	root := testutil.Registry(t)
	testutil.Write(t, root, "sandboxes/a.yaml", "name: b\nowner: x\nbaseline: dev\nttl: 1h\nservices: [{name: orders}]\n")
	if _, err := registry.Load(root); err == nil || !strings.Contains(err.Error(), "must match the file name") {
		t.Fatalf("got %v", err)
	}
}

func TestParseTTL(t *testing.T) {
	for in, ok := range map[string]bool{"72h": true, "3d": true, "90m": true, "": false, "0h": false, "-1h": false, "xd": false} {
		_, err := registry.ParseTTL(in)
		if (err == nil) != ok {
			t.Errorf("%q: err=%v", in, err)
		}
	}
}

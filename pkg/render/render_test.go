package render_test

import (
	"strings"
	"testing"

	"github.com/mcafeelabs/platform/internal/testutil"
	"github.com/mcafeelabs/platform/pkg/registry"
	"github.com/mcafeelabs/platform/pkg/render"
	"github.com/mcafeelabs/platform/pkg/values"
	"sigs.k8s.io/yaml"
)

func renderer(t *testing.T, root string) *render.Renderer {
	t.Helper()
	reg, err := registry.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	rd, err := render.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	return rd
}

func get(t *testing.T, r render.Result, path ...string) any {
	t.Helper()
	v, ok := values.Get(r.Values, path...)
	if !ok {
		b, _ := yaml.Marshal(r.Values)
		t.Fatalf("%s missing in:\n%s", strings.Join(path, "."), b)
	}
	return v
}

func TestRenderDev(t *testing.T) {
	rd := renderer(t, testutil.Registry(t))
	r, err := rd.Service("orders", render.Target{Env: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Path() != "rendered/dev/orders/values.yaml" {
		t.Fatal(r.Path())
	}
	// Deterministic outputs become literals.
	if got := get(t, r, "config", "payments", "url"); got != "http://payments.dev.svc.cluster.local" {
		t.Fatalf("payments url %v", got)
	}
	if got := get(t, r, "config", "events", "stockChanged"); got != "stock.changed" {
		t.Fatalf("subject %v", got)
	}
	if got := get(t, r, "config", "events", "queue"); got != "dev-orders-events-queue" {
		t.Fatalf("queue %v", got)
	}
	// Dynamic outputs become $kv references to the outputs store.
	if got := get(t, r, "config", "database", "password", "$kv"); got != "ssm://services/dev/orders/db#password" {
		t.Fatalf("db password %v", got)
	}
	// Global layer and wave.
	if get(t, r, "config", "log", "level") != "info" || get(t, r, "service", "wave") != 2 {
		t.Fatal("global layer or wave")
	}
	// The config template holds every reference exactly once.
	tpl := get(t, r, "configFile", "template").(string)
	refs := get(t, r, "configFile", "refs").([]any)
	if len(refs) != 7 || strings.Count(tpl, "| toJson }}") != 7 {
		t.Fatalf("refs %d, template:\n%s", len(refs), tpl)
	}
	if !strings.Contains(tpl, "url: http://payments.dev.svc.cluster.local") {
		t.Fatalf("template lost literals:\n%s", tpl)
	}
	// keyStyle dotted maps provider keys to Secret names for the localdev store.
	first := refs[0].(map[string]any)
	if first["key"] != "services.dev.orders.db" || first["store"].(map[string]any)["name"] != "localdev-params" {
		t.Fatalf("ref0 %v", first)
	}
	if get(t, r, "image", "repository") != "ghcr.io/mcafeelabs/orders" || get(t, r, "image", "tag") != "0.1.0" {
		t.Fatal("image")
	}
}

func TestLayer4Overrides(t *testing.T) {
	rd := renderer(t, testutil.Registry(t))
	r, err := rd.Service("orders", render.Target{Env: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if get(t, r, "config", "log", "level") != "warn" || get(t, r, "deployment", "replicas") != float64(2) {
		t.Fatal("service x environment layer did not apply")
	}
	if get(t, r, "config", "payments", "url") != "http://payments.staging.svc.cluster.local" {
		t.Fatal("staging namespace")
	}
}

func TestSandboxResolution(t *testing.T) {
	root := testutil.Registry(t)
	rd := renderer(t, root)
	sb := rd.Reg.Sandboxes["ryan-checkout-v2"]
	r, err := rd.Service("orders", render.Target{Env: "dev", Sandbox: sb})
	if err != nil {
		t.Fatal(err)
	}
	if r.Path() != "rendered/sbx/ryan-checkout-v2/orders/values.yaml" {
		t.Fatal(r.Path())
	}
	if get(t, r, "service", "namespace") != "sbx-ryan-checkout-v2" || get(t, r, "service", "sandbox", "baselineNamespace") != "dev" {
		t.Fatal("sandbox namespaces")
	}
	// payments runs in the sandbox, but api is shared: it resolves to the
	// baseline host and the waypoint routes tagged requests into the sandbox.
	if got := get(t, r, "config", "payments", "url"); got != "http://payments.dev.svc.cluster.local" {
		t.Fatalf("shared output %v", got)
	}
	// orders' own db is isolated: the sandbox gets its own copy.
	if got := get(t, r, "config", "database", "host", "$kv"); got != "ssm://sandboxes/ryan-checkout-v2/orders/db#host" {
		t.Fatalf("isolated output %v", got)
	}
	if got := get(t, r, "config", "events", "queue"); got != "dev-sbx-ryan-checkout-v2-orders-events-queue" {
		t.Fatalf("isolated queue %v", got)
	}
	// Image from the sandbox file.
	if get(t, r, "image", "tag") != "pr-1" || get(t, r, "image", "repository") != "ghcr.io/mcafeelabs/orders" {
		t.Fatal("sandbox image")
	}
	// Only isolated outputs are provisioned in the sandbox.
	for _, o := range get(t, r, "outputs").([]any) {
		m := o.(map[string]any)
		if want := m["sandbox"] == "isolate"; m["provision"] != want {
			t.Fatalf("output %v provision=%v", m["name"], m["provision"])
		}
	}
	// Layer 5 values.
	p, err := rd.Service("payments", render.Target{Env: "dev", Sandbox: sb})
	if err != nil {
		t.Fatal(err)
	}
	if get(t, p, "config", "featureFlags", "refundsV2") != true {
		t.Fatal("sandbox values layer")
	}
}

func TestIsolatedOutputOfServiceNotInSandboxResolvesToBaseline(t *testing.T) {
	root := testutil.Registry(t)
	// reports reads orders' isolated db; a sandbox running only reports must
	// use the baseline copy, since orders is not in the sandbox.
	testutil.Write(t, root, "services/reports/service.yaml", `
apiVersion: platform/v1
name: reports
owner: team-data
chartProfile: worker
consumes: [{service: orders, output: db}]
config: {schema: ./schema.yaml}
`)
	testutil.Write(t, root, "services/reports/schema.yaml", "type: object\n")
	testutil.Write(t, root, "services/reports/values.yaml", `
image: {tag: "1.0.0"}
config:
  ordersDB: {$output: orders/db#host}
`)
	testutil.Write(t, root, "sandboxes/reports-only.yaml", "name: reports-only\nowner: x\nbaseline: dev\nttl: 1h\nservices: [{name: reports}]\n")
	rd := renderer(t, root)
	r, err := rd.Service("reports", render.Target{Env: "dev", Sandbox: rd.Reg.Sandboxes["reports-only"]})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, "config", "ordersDB", "$kv"); got != "ssm://services/dev/orders/db#host" {
		t.Fatalf("got %v", got)
	}
	if get(t, r, "service", "wave") != 3 {
		t.Fatal("reports should deploy after orders")
	}
}

func TestRenderErrors(t *testing.T) {
	cases := map[string]struct {
		file, content string
		want          []string
	}{
		"output not in consumes": {
			"services/identity/environments/dev/values.yaml",
			"config: {peer: {$output: payments/api}}\n",
			[]string{"add {service: payments, output: api} to consumes"},
		},
		"unknown output": {
			"services/payments/environments/dev/values.yaml",
			"config: {identity: {url: {$output: identity/grpc}}}\n",
			[]string{"identity does not provide \"grpc\""},
		},
		"dynamic output without key": {
			"services/orders/environments/dev/values.yaml",
			"config: {database: {host: {$output: orders/db}}}\n",
			[]string{"pick a key with #<key>"},
		},
		"literal password": {
			"services/orders/environments/dev/values.yaml",
			"config: {database: {password: hunter2}}\n",
			[]string{"config.database.password: must be a reference"},
		},
		"schema violation": {
			"services/payments/environments/dev/values.yaml",
			"config: {currency: GBP, featureFlags: {typo: true}}\n",
			[]string{"config.currency", "config.featureFlags"},
		},
		"unknown provider": {
			"services/identity/environments/dev/values.yaml",
			"config: {signingKey: {$secret: vault://identity/key}}\n",
			[]string{"provider \"vault\" has no entry in platform.secretStores"},
		},
		"unknown top-level key": {
			"services/identity/environments/dev/values.yaml",
			"replicas: 3\n",
			[]string{"unknown top-level key \"replicas\""},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := testutil.Registry(t)
			testutil.Write(t, root, c.file, c.content)
			rd := renderer(t, root)
			_, err := rd.Env("dev")
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("missing %q in:\n%v", w, err)
				}
			}
		})
	}
}

func TestNullDeletesKey(t *testing.T) {
	root := testutil.Registry(t)
	testutil.Write(t, root, "services/orders/environments/dev/values.yaml", "config: {featureFlags: null}\n")
	rd := renderer(t, root)
	r, err := rd.Service("orders", render.Target{Env: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values.Get(r.Values, "config", "featureFlags"); ok {
		t.Fatal("null did not delete the key")
	}
	if len(get(t, r, "configFile", "refs").([]any)) != 6 {
		t.Fatal("deleted reference still rendered")
	}
}

func TestTemplateEscapesLiteralBraces(t *testing.T) {
	root := testutil.Registry(t)
	testutil.Write(t, root, "services/identity/environments/dev/values.yaml", "config: {greeting: \"{{ not a template }}\"}\n")
	rd := renderer(t, root)
	r, err := rd.Service("identity", render.Target{Env: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	tpl := get(t, r, "configFile", "template").(string)
	if !strings.Contains(tpl, `{{ "{{" }} not a template }}`) {
		t.Fatalf("braces not escaped:\n%s", tpl)
	}
}

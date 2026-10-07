package generate_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mcafeelabs/platform/internal/testutil"
	"github.com/mcafeelabs/platform/pkg/generate"
	"github.com/mcafeelabs/platform/pkg/registry"
	"sigs.k8s.io/yaml"
)

func gen(t *testing.T, root string, opts generate.Options) generate.Files {
	t.Helper()
	reg, err := registry.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := generate.Generate(reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func docs(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, part := range strings.Split(string(b), "\n---\n") {
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(part), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestLayout(t *testing.T) {
	files := gen(t, testutil.Registry(t), generate.Options{})
	for _, p := range []string{
		"rendered/dev/orders/values.yaml",
		"rendered/staging/identity/values.yaml",
		"rendered/sbx/ryan-checkout-v2/orders/values.yaml",
		"rendered/sbx/ryan-checkout-v2/payments/values.yaml",
		"apps/dev/namespace.yaml",
		"apps/dev/waypoint.yaml",
		"apps/dev/sandbox-index.yaml",
		"apps/dev/orders.yaml",
		"apps/_sandboxes/ryan-checkout-v2/routing.yaml",
		"apps/_platform/applicationsets.yaml",
		"apps/_platform/kargo.yaml",
	} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	if _, ok := files["rendered/sbx/ryan-checkout-v2/identity/values.yaml"]; ok {
		t.Error("sandbox rendered a service it does not run")
	}
}

func TestSyncWavesFollowTheGraph(t *testing.T) {
	files := gen(t, testutil.Registry(t), generate.Options{})
	want := map[string]string{"identity": "0", "inventory": "0", "payments": "1", "orders": "2"}
	for svc, w := range want {
		app := docs(t, files["apps/dev/"+svc+".yaml"])[0]
		ann := app["metadata"].(map[string]any)["annotations"].(map[string]any)
		if ann["argocd.argoproj.io/sync-wave"] != w {
			t.Errorf("%s wave %v want %s", svc, ann["argocd.argoproj.io/sync-wave"], w)
		}
	}
}

func TestKargoDeliveredEnvReadsStageBranch(t *testing.T) {
	files := gen(t, testutil.Registry(t), generate.Options{})
	app := docs(t, files["apps/staging/orders.yaml"])[0]
	meta := app["metadata"].(map[string]any)
	if meta["annotations"].(map[string]any)["kargo.akuity.io/authorized-stage"] != "services:orders-staging" {
		t.Fatalf("annotations %v", meta["annotations"])
	}
	sources := app["spec"].(map[string]any)["sources"].([]any)
	if sources[1].(map[string]any)["targetRevision"] != "stage/staging/orders" {
		t.Fatalf("values source %v", sources[1])
	}
	// dev is delivered directly from main.
	dev := docs(t, files["apps/dev/orders.yaml"])[0]
	if dev["spec"].(map[string]any)["sources"].([]any)[1].(map[string]any)["targetRevision"] != "main" {
		t.Fatal("dev should read main")
	}

	kinds := map[string]int{}
	for _, d := range docs(t, files["apps/_platform/kargo.yaml"]) {
		kinds[d["kind"].(string)]++
		if d["kind"] == "Stage" && d["metadata"].(map[string]any)["name"] == "orders-staging" {
			steps := d["spec"].(map[string]any)["promotionTemplate"].(map[string]any)["spec"].(map[string]any)["steps"].([]any)
			var uses []string
			for _, s := range steps {
				uses = append(uses, s.(map[string]any)["uses"].(string))
			}
			if got := strings.Join(uses, ","); got != "git-clone,git-clear,copy,yaml-update,git-commit,git-push,argocd-update" {
				t.Fatalf("steps %s", got)
			}
		}
	}
	if kinds["Project"] != 1 || kinds["ProjectConfig"] != 1 || kinds["Warehouse"] != 4 || kinds["Stage"] != 4 {
		t.Fatalf("kargo kinds %v", kinds)
	}
}

func TestBaggageRegex(t *testing.T) {
	re := regexp.MustCompile(generate.BaggageRegex("ryan-checkout-v2"))
	for header, want := range map[string]bool{
		"sandbox=ryan-checkout-v2":                    true,
		"userId=7,sandbox=ryan-checkout-v2":           true,
		"userId=7, sandbox=ryan-checkout-v2 ,other=1": true,
		"sandbox=ryan-checkout-v2;ttl=1":              true,
		"sandbox=ryan-checkout-v20":                   false,
		"mysandbox=ryan-checkout-v2":                  false,
		"sandbox=other":                               false,
		"":                                            false,
	} {
		if re.MatchString(header) != want {
			t.Errorf("%q: want %v", header, want)
		}
	}
}

func TestRoutingOnlyForOverriddenHTTPServices(t *testing.T) {
	files := gen(t, testutil.Registry(t), generate.Options{})
	var routes []string
	for _, d := range docs(t, files["apps/_sandboxes/ryan-checkout-v2/routing.yaml"]) {
		if d["kind"] == "HTTPRoute" {
			routes = append(routes, d["metadata"].(map[string]any)["name"].(string))
			if d["metadata"].(map[string]any)["namespace"] != "dev" {
				t.Error("routes live in the baseline namespace")
			}
		}
	}
	if got := strings.Join(routes, ","); got != "sbx-ryan-checkout-v2-orders,sbx-ryan-checkout-v2-payments,sbx-ryan-checkout-v2-ingress" {
		t.Fatalf("routes %s", got)
	}
	idx := docs(t, files["apps/dev/sandbox-index.yaml"])[0]
	if !strings.Contains(idx["data"].(map[string]any)["sandboxes.json"].(string), `"orders"`) {
		t.Fatal("sandbox index")
	}
}

func TestExpiry(t *testing.T) {
	root := testutil.Registry(t)
	testutil.Write(t, root, "sandboxes/old.yaml", "name: old\nowner: x\nbaseline: dev\nttl: 1h\ncreated: \"2026-01-01T00:00:00Z\"\nservices: [{name: identity}]\n")
	reg, _ := registry.Load(root)
	now := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	if exp, at := generate.Expired(reg.Sandboxes["old"], now, nil); !exp || at.Hour() != 1 {
		t.Fatalf("old: %v %v", exp, at)
	}
	created := func(string) (time.Time, bool) { return now, true }
	if exp, _ := generate.Expired(reg.Sandboxes["ryan-checkout-v2"], now, created); exp {
		t.Fatal("fresh sandbox expired")
	}
	files := gen(t, root, generate.Options{Now: now})
	if !strings.Contains(string(files["apps/_sandboxes/old/routing.yaml"]), "platform.mcafeelabs.io/expires: \"2026-01-01T01:00:00Z\"") {
		t.Fatal("expires annotation")
	}
}

func TestWriteAndDiff(t *testing.T) {
	root := testutil.Registry(t)
	files := gen(t, root, generate.Options{})
	if err := generate.Write(root, files); err != nil {
		t.Fatal(err)
	}
	if d, err := generate.Diff(root, files); err != nil || len(d) != 0 {
		t.Fatalf("diff after write: %v %v", d, err)
	}
	stale := filepath.Join(root, "rendered", "dev", "ghost", "values.yaml")
	_ = os.MkdirAll(filepath.Dir(stale), 0o755)
	_ = os.WriteFile(stale, []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "apps", "dev", "orders.yaml"), []byte("edited"), 0o644)
	d, _ := generate.Diff(root, files)
	if got := strings.Join(d, "|"); got != "changed: apps/dev/orders.yaml|stale:   rendered/dev/ghost/values.yaml" {
		t.Fatalf("diff %s", got)
	}
	// Write removes stale files.
	if err := generate.Write(root, files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale file survived Write")
	}
}

func TestDeterministic(t *testing.T) {
	root := testutil.Registry(t)
	a := gen(t, root, generate.Options{})
	b := gen(t, root, generate.Options{})
	for p := range a {
		if string(a[p]) != string(b[p]) {
			t.Fatalf("%s differs between runs", p)
		}
	}
}

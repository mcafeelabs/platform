package values

import (
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

func y(t *testing.T, s string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMergeHelmSemantics(t *testing.T) {
	base := y(t, `
a: {x: 1, y: 2, nested: {keep: true, drop: 1}}
list: [1, 2, 3]
scalar: old
typechange: {k: v}
gone: here
`)
	overlay := y(t, `
a: {y: 20, z: 30, nested: {drop: null}}
list: [9]
scalar: new
typechange: now-a-string
gone: null
fresh: {inner: null, kept: 1}
`)
	got := Merge(base, overlay)
	want := y(t, `
a: {x: 1, y: 20, z: 30, nested: {keep: true}}
list: [9]
scalar: new
typechange: now-a-string
fresh: {kept: 1}
`)
	if !reflect.DeepEqual(got, want) {
		gb, _ := yaml.Marshal(got)
		t.Fatalf("merge mismatch:\n%s", gb)
	}
	// Inputs are untouched.
	if base["scalar"] != "old" || base["gone"] != "here" {
		t.Fatal("base was modified")
	}
}

func TestMergeAllLaterWins(t *testing.T) {
	got := MergeAll(y(t, "v: 1"), y(t, "v: 2"), y(t, "w: 3"), nil)
	if got["v"] != float64(2) || got["w"] != float64(3) {
		t.Fatalf("got %v", got)
	}
}

func TestGetters(t *testing.T) {
	m := y(t, "platform: {namespace: dev, nested: {flag: true}}")
	if GetString(m, "platform", "namespace") != "dev" {
		t.Fatal("GetString")
	}
	if len(GetMap(m, "platform", "nested")) != 1 || len(GetMap(m, "missing")) != 0 {
		t.Fatal("GetMap")
	}
	if _, ok := Get(m, "platform", "namespace", "deeper"); ok {
		t.Fatal("Get through a scalar")
	}
}

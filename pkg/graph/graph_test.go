package graph

import (
	"reflect"
	"strings"
	"testing"
)

func TestWavesAreLongestPath(t *testing.T) {
	g := New(map[string][]string{
		"identity":  nil,
		"inventory": nil,
		"payments":  {"identity"},
		"orders":    {"payments", "inventory", "identity"},
		"reports":   {"orders"},
	})
	w, err := g.Waves()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"identity": 0, "inventory": 0, "payments": 1, "orders": 2, "reports": 3}
	if !reflect.DeepEqual(w, want) {
		t.Fatalf("waves %v", w)
	}
	order, _ := g.Order()
	if !reflect.DeepEqual(order, []string{"identity", "inventory", "payments", "orders", "reports"}) {
		t.Fatalf("order %v", order)
	}
	if got := g.Dependents("identity"); !reflect.DeepEqual(got, []string{"orders", "payments", "reports"}) {
		t.Fatalf("dependents %v", got)
	}
	if got := g.Dependents("reports"); len(got) != 0 {
		t.Fatalf("leaf dependents %v", got)
	}
	if !strings.Contains(g.DOT(), `"orders" -> "payments"`) {
		t.Fatal("DOT edges")
	}
}

func TestCycleIsNamed(t *testing.T) {
	g := New(map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
		"d": nil,
	})
	c := g.Cycle()
	if c == nil {
		t.Fatal("expected a cycle")
	}
	if c.Error() != "deployment graph has a cycle: a -> b -> c -> a (each depends on the next)" {
		t.Fatalf("got %q", c.Error())
	}
	if _, err := g.Waves(); err == nil {
		t.Fatal("Waves should fail on a cycle")
	}
}

func TestSelfLoopIsACycle(t *testing.T) {
	if New(map[string][]string{"a": {"a"}}).Cycle() == nil {
		t.Fatal("self loop")
	}
}

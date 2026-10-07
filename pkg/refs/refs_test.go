package refs

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      any
		isRef   bool
		wantErr string
		check   func(Ref) bool
	}{
		{"literal", false, "", nil},
		{map[string]any{"a": 1}, false, "", nil},
		{map[string]any{"$secret": "aws-sm://orders/db#password"}, true, "", func(r Ref) bool {
			return r.Kind == Secret && r.Provider == "aws-sm" && r.Key == "orders/db" && r.Property == "password"
		}},
		{map[string]any{"$kv": "ssm://orders/flags/checkout-v2"}, true, "", func(r Ref) bool {
			return r.Kind == KV && r.Provider == "ssm" && r.Key == "orders/flags/checkout-v2" && r.Property == ""
		}},
		{map[string]any{"$output": "payments/api"}, true, "", func(r Ref) bool {
			return r.Kind == Output && r.Service == "payments" && r.Output == "api"
		}},
		{map[string]any{"$output": "inventory/stock.changed"}, true, "", func(r Ref) bool {
			return r.Output == "stock.changed"
		}},
		{map[string]any{"$output": "orders/db#host"}, true, "", func(r Ref) bool { return r.Property == "host" }},
		{map[string]any{"$secret": "no-scheme"}, true, "must look like", nil},
		{map[string]any{"$output": "payments"}, true, "must look like", nil},
		{map[string]any{"$secret": 3}, true, "must be a string", nil},
		{map[string]any{"$vault": "x://y"}, true, "unknown reference", nil},
		{map[string]any{"$secret": "a://b", "extra": 1}, true, "exactly one key", nil},
	}
	for _, c := range cases {
		r, isRef, err := Parse(c.in)
		if isRef != c.isRef {
			t.Errorf("%v: isRef=%v want %v", c.in, isRef, c.isRef)
		}
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%v: err=%v want %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: unexpected %v", c.in, err)
		}
		if c.check != nil && !c.check(r) {
			t.Errorf("%v: parsed %+v", c.in, r)
		}
	}
}

func TestWalkAndReplace(t *testing.T) {
	tree := map[string]any{
		"db":   map[string]any{"password": map[string]any{"$secret": "aws-sm://db#pw"}},
		"urls": []any{map[string]any{"$output": "payments/api"}, "literal"},
		"bad":  map[string]any{"$nope": "x"},
	}
	found, errs := Walk(tree)
	if len(found) != 2 || len(errs) != 1 {
		t.Fatalf("found=%v errs=%v", found, errs)
	}
	if PathString(found[0].Path) != "db.password" || PathString(found[1].Path) != "urls[0]" {
		t.Fatalf("paths: %v %v", found[0].Path, found[1].Path)
	}
	delete(tree, "bad")
	out, err := Replace(tree, func(path []string, r Ref) (any, error) { return "<" + string(r.Kind) + ">", nil })
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["db"].(map[string]any)["password"] != "<secret>" || m["urls"].([]any)[0] != "<output>" {
		t.Fatalf("replace: %v", m)
	}
	if _, isRef, _ := Parse(tree["db"].(map[string]any)["password"]); !isRef {
		t.Fatal("Replace modified its input")
	}
}

// Package refs parses config values that reference something instead of
// holding a literal:
//
//	$secret: aws-sm://orders/db#password   a secret store entry
//	$kv:     ssm://orders/flags/checkout-v2 a key-value (parameter) entry
//	$output: payments/api                   another service's output
//
// A reference is a map with exactly one of those keys.
package refs

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind of reference.
type Kind string

const (
	Secret Kind = "secret"
	KV     Kind = "kv"
	Output Kind = "output"
)

// Keys maps the YAML key to the reference kind.
var Keys = map[string]Kind{"$secret": Secret, "$kv": KV, "$output": Output}

// Ref is one parsed reference.
type Ref struct {
	Kind Kind
	// Secret and KV.
	Provider string
	Key      string
	// Output.
	Service string
	Output  string
	// Property selects one field (the part after #). Optional.
	Property string
	// Raw is the string as written.
	Raw string
}

var (
	storeURI  = regexp.MustCompile(`^([a-z][a-z0-9-]*)://([^#\s]+)(?:#([A-Za-z0-9_.-]+))?$`)
	outputRef = regexp.MustCompile(`^([a-z][-a-z0-9]*)/([a-z0-9][-.a-z0-9]*)(?:#([A-Za-z0-9_.-]+))?$`)
)

// String renders the reference back to its YAML form.
func (r Ref) String() string { return r.Raw }

// YAMLKey returns the map key used for this kind ($secret, $kv, $output).
func (r Ref) YAMLKey() string { return "$" + string(r.Kind) }

// Map returns the reference as it appears in YAML.
func (r Ref) Map() map[string]any { return map[string]any{r.YAMLKey(): r.Raw} }

// Parse returns the reference held by v, ok=false if v is not a reference,
// or an error if v looks like one but is malformed.
func Parse(v any) (Ref, bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Ref{}, false, nil
	}
	var refKeys []string
	for k := range m {
		if strings.HasPrefix(k, "$") {
			refKeys = append(refKeys, k)
		}
	}
	if len(refKeys) == 0 {
		return Ref{}, false, nil
	}
	sort.Strings(refKeys)
	if len(m) != 1 {
		return Ref{}, true, fmt.Errorf("a reference must be a map with exactly one key, got %s", strings.Join(sortedKeys(m), ", "))
	}
	k := refKeys[0]
	kind, known := Keys[k]
	if !known {
		return Ref{}, true, fmt.Errorf("unknown reference %q (want $secret, $kv or $output)", k)
	}
	s, isStr := m[k].(string)
	if !isStr {
		return Ref{}, true, fmt.Errorf("%s must be a string", k)
	}
	r := Ref{Kind: kind, Raw: s}
	switch kind {
	case Secret, KV:
		g := storeURI.FindStringSubmatch(s)
		if g == nil {
			return Ref{}, true, fmt.Errorf("%s %q must look like <provider>://<key>[#<property>]", k, s)
		}
		r.Provider, r.Key, r.Property = g[1], strings.TrimPrefix(g[2], "/"), g[3]
	case Output:
		g := outputRef.FindStringSubmatch(s)
		if g == nil {
			return Ref{}, true, fmt.Errorf("$output %q must look like <service>/<output>[#<key>]", s)
		}
		r.Service, r.Output, r.Property = g[1], g[2], g[3]
	}
	return r, true, nil
}

// Found is a reference and where it sits in the tree.
type Found struct {
	Path []string
	Ref  Ref
}

// PathString renders a path as a.b[0].c.
func PathString(path []string) string {
	var b strings.Builder
	for i, p := range path {
		if strings.HasPrefix(p, "[") {
			b.WriteString(p)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(p)
	}
	return b.String()
}

// Walk finds every reference in a tree, in a stable order. Malformed
// references are returned as errors naming their path.
func Walk(tree any) ([]Found, []error) {
	var found []Found
	var errs []error
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		r, isRef, err := Parse(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", PathString(path), err))
			return
		}
		if isRef {
			found = append(found, Found{Path: append([]string(nil), path...), Ref: r})
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(x) {
				walk(x[k], append(path, k))
			}
		case []any:
			for i, e := range x {
				walk(e, append(path, "["+strconv.Itoa(i)+"]"))
			}
		}
	}
	walk(tree, nil)
	return found, errs
}

// Replace returns a copy of tree with every reference passed through fn.
// fn returns the value to put in the reference's place.
func Replace(tree any, fn func(path []string, r Ref) (any, error)) (any, error) {
	var walk func(v any, path []string) (any, error)
	walk = func(v any, path []string) (any, error) {
		r, isRef, err := Parse(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", PathString(path), err)
		}
		if isRef {
			return fn(append([]string(nil), path...), r)
		}
		switch x := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(x))
			for _, k := range sortedKeys(x) {
				nv, err := walk(x[k], append(path, k))
				if err != nil {
					return nil, err
				}
				out[k] = nv
			}
			return out, nil
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				nv, err := walk(e, append(path, "["+strconv.Itoa(i)+"]"))
				if err != nil {
					return nil, err
				}
				out[i] = nv
			}
			return out, nil
		default:
			return v, nil
		}
	}
	return walk(tree, nil)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

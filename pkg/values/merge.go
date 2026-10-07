// Package values merges config layers with Helm's semantics: maps deep-merge,
// scalars and lists replace, and null deletes a key.
package values

// Merge returns base with overlay applied. Neither input is modified.
//
// Helm coalesces user values onto chart defaults the same way: a key set to
// null in a later layer removes it, a map merges key by key, and anything else
// (scalars, lists, or a type change) replaces the earlier value wholesale.
func Merge(base, overlay map[string]any) map[string]any {
	out, _ := DeepCopy(base).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range overlay {
		if v == nil {
			delete(out, k)
			continue
		}
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = Merge(bm, om)
				continue
			}
			out[k] = stripNulls(DeepCopy(om).(map[string]any))
			continue
		}
		out[k] = DeepCopy(v)
	}
	return out
}

// MergeAll merges layers in order; later layers win.
func MergeAll(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		out = Merge(out, l)
	}
	return out
}

// stripNulls drops null-valued keys from a map that had nothing to delete
// from, matching Helm (a null never survives into the final values).
func stripNulls(m map[string]any) map[string]any {
	for k, v := range m {
		switch x := v.(type) {
		case nil:
			delete(m, k)
		case map[string]any:
			m[k] = stripNulls(x)
		}
	}
	return m
}

// DeepCopy copies maps and slices recursively; other values are shared.
func DeepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = DeepCopy(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = DeepCopy(vv)
		}
		return out
	default:
		return v
	}
}

// Get walks a dotted path through nested maps.
func Get(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// GetMap returns the map at path, or an empty map.
func GetMap(m map[string]any, path ...string) map[string]any {
	v, ok := Get(m, path...)
	if !ok {
		return map[string]any{}
	}
	mm, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return mm
}

// GetString returns the string at path, or "".
func GetString(m map[string]any, path ...string) string {
	v, _ := Get(m, path...)
	s, _ := v.(string)
	return s
}

// Package schema validates a service's merged config against its
// config-values.schema.yaml (JSON Schema written in YAML).
//
// Besides standard JSON Schema, a field may declare which reference kinds it
// accepts with x-ref, e.g. `x-ref: [secret]`. A field with x-ref only accepts
// those references; add `literal` to the list to also allow a plain value.
// Value constraints (format, pattern, ...) apply to literals only: a
// reference's value is unknown until the ExternalSecret resolves it.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mcafeelabs/platform/pkg/refs"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"sigs.k8s.io/yaml"
)

// Schema is a compiled config schema plus its raw document for x-ref lookups.
type Schema struct {
	compiled *jsonschema.Schema
	doc      map[string]any
	path     string
}

// Load reads and compiles a schema file. Compilation validates the schema
// against the JSON Schema metaschema (draft 2020-12 unless $schema says
// otherwise).
func Load(path string) (*Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Compile(path, b)
}

// Compile compiles a schema from YAML or JSON bytes. name is used in errors.
func Compile(name string, b []byte) (*Schema, error) {
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	doc, ok := inst.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: schema must be an object", name)
	}
	if err := checkXRef(doc, nil); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	abs, _ := filepath.Abs(name)
	url := "file://" + filepath.ToSlash(abs)
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid JSON Schema: %w", name, err)
	}
	return &Schema{compiled: s, doc: doc, path: name}, nil
}

var xrefKinds = map[string]bool{"secret": true, "kv": true, "output": true, "literal": true}

func checkXRef(v any, path []string) error {
	switch x := v.(type) {
	case map[string]any:
		if xr, ok := x["x-ref"]; ok {
			list, ok := xr.([]any)
			if !ok || len(list) == 0 {
				return fmt.Errorf("%s: x-ref must be a non-empty list", strings.Join(path, "/"))
			}
			for _, k := range list {
				s, _ := k.(string)
				if !xrefKinds[s] {
					return fmt.Errorf("%s: x-ref entry %v must be one of secret, kv, output, literal", strings.Join(path, "/"), k)
				}
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := checkXRef(x[k], append(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range x {
			if err := checkXRef(e, append(path, strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

// node returns the schema node describing an instance path, following
// properties, items, additionalProperties and local $refs. nil if the schema
// does not describe the path.
func (s *Schema) node(path []string) map[string]any {
	cur := s.doc
	for _, p := range path {
		cur = s.deref(cur)
		if cur == nil {
			return nil
		}
		var next map[string]any
		if strings.HasPrefix(p, "[") {
			next, _ = cur["items"].(map[string]any)
		} else {
			if props, ok := cur["properties"].(map[string]any); ok {
				next, _ = props[p].(map[string]any)
			}
			if next == nil {
				next, _ = cur["additionalProperties"].(map[string]any)
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return s.deref(cur)
}

func (s *Schema) deref(n map[string]any) map[string]any {
	for i := 0; n != nil && i < 16; i++ {
		ref, ok := n["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return n
		}
		var cur any = s.doc
		for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
			m, _ := cur.(map[string]any)
			cur = m[tok]
		}
		n, _ = cur.(map[string]any)
	}
	return n
}

// xref returns the reference kinds allowed at path, or nil if unrestricted.
func (s *Schema) xref(path []string) []string {
	n := s.node(path)
	if n == nil {
		return nil
	}
	list, _ := n["x-ref"].([]any)
	out := make([]string, 0, len(list))
	for _, k := range list {
		out = append(out, k.(string))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// CheckRefs enforces x-ref on the unresolved config: every reference must be
// of a kind its field accepts, and a field restricted to references must not
// hold a literal.
func (s *Schema) CheckRefs(config map[string]any) []error {
	var errs []error
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		r, isRef, err := refs.Parse(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("config.%s: %w", refs.PathString(path), err))
			return
		}
		allowed := s.xref(path)
		if isRef {
			if len(allowed) > 0 && !contains(allowed, string(r.Kind)) {
				errs = append(errs, fmt.Errorf("config.%s: %s reference not allowed here (x-ref: %s)", refs.PathString(path), r.YAMLKey(), strings.Join(allowed, ", ")))
			}
			return
		}
		switch x := v.(type) {
		case map[string]any:
			if len(allowed) > 0 && !contains(allowed, "literal") {
				errs = append(errs, fmt.Errorf("config.%s: must be a reference (x-ref: %s), not a literal", refs.PathString(path), strings.Join(allowed, ", ")))
				return
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k], append(path, k))
			}
		case []any:
			if len(allowed) > 0 && !contains(allowed, "literal") {
				errs = append(errs, fmt.Errorf("config.%s: must be a reference (x-ref: %s), not a literal", refs.PathString(path), strings.Join(allowed, ", ")))
				return
			}
			for i, e := range x {
				walk(e, append(path, "["+strconv.Itoa(i)+"]"))
			}
		default:
			if len(path) > 0 && len(allowed) > 0 && !contains(allowed, "literal") {
				errs = append(errs, fmt.Errorf("config.%s: must be a reference (x-ref: %s), not a literal", refs.PathString(path), strings.Join(allowed, ", ")))
			}
		}
	}
	walk(config, nil)
	return errs
}

// placeholder stands in for a reference's value during validation.
const placeholder = "__svcreg_ref__"

// Validate checks config (whose remaining references will resolve to strings)
// against the schema. Errors located at a reference are dropped unless they
// are type errors: a reference always yields a string.
func (s *Schema) Validate(config map[string]any) []error {
	refPaths := map[string]bool{}
	replaced, err := refs.Replace(config, func(path []string, _ refs.Ref) (any, error) {
		refPaths[pointer(path)] = true
		return placeholder, nil
	})
	if err != nil {
		return []error{err}
	}
	// Round-trip through JSON so numbers have the types the validator expects.
	j, err := json.Marshal(replaced)
	if err != nil {
		return []error{err}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		return []error{err}
	}
	verr := s.compiled.Validate(inst)
	if verr == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return []error{verr}
	}
	var out []error
	for _, leaf := range leaves(ve) {
		loc := "/" + strings.Join(leaf.InstanceLocation, "/")
		if refPaths[loc] && !isTypeError(leaf) {
			continue
		}
		where := "config"
		if len(leaf.InstanceLocation) > 0 {
			where = "config." + strings.Join(leaf.InstanceLocation, ".")
		}
		out = append(out, fmt.Errorf("%s: %s", where, leaf.ErrorKind.LocalizedString(printer)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Error() < out[j].Error() })
	return out
}

func pointer(path []string) string {
	toks := make([]string, len(path))
	for i, p := range path {
		toks[i] = strings.TrimSuffix(strings.TrimPrefix(p, "["), "]")
	}
	return "/" + strings.Join(toks, "/")
}

func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

var printer = message.NewPrinter(language.English)

func isTypeError(e *jsonschema.ValidationError) bool {
	kp := e.ErrorKind.KeywordPath()
	return len(kp) > 0 && kp[len(kp)-1] == "type"
}

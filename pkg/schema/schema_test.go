package schema

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const doc = `
$schema: https://json-schema.org/draft/2020-12/schema
type: object
required: [db, url]
$defs:
  secretString: {type: string, x-ref: [secret]}
properties:
  url: {type: string, pattern: "^https?://"}
  port: {type: integer}
  db:
    type: object
    properties:
      password: {$ref: "#/$defs/secretString"}
      host: {type: string, x-ref: [output, literal]}
  hosts:
    type: array
    items: {type: string, x-ref: [kv]}
`

func compile(t *testing.T) *Schema {
	t.Helper()
	s, err := Compile("test.schema.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func cfg(t *testing.T, s string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func joined(errs []error) string {
	var s []string
	for _, e := range errs {
		s = append(s, e.Error())
	}
	return strings.Join(s, "\n")
}

func TestCheckRefs(t *testing.T) {
	s := compile(t)
	ok := cfg(t, `
url: https://x
db:
  password: {$secret: aws-sm://db#pw}
  host: {$output: db/primary#host}
hosts: [{$kv: ssm://h1}]
`)
	if errs := s.CheckRefs(ok); len(errs) > 0 {
		t.Fatalf("unexpected: %s", joined(errs))
	}
	bad := cfg(t, `
db:
  password: hunter2
  host: {$secret: aws-sm://h}
hosts: [literal]
`)
	got := joined(s.CheckRefs(bad))
	for _, want := range []string{
		"config.db.password: must be a reference (x-ref: secret), not a literal",
		"config.db.host: $secret reference not allowed here (x-ref: output, literal)",
		"config.hosts[0]: must be a reference (x-ref: kv), not a literal",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestValidateIgnoresConstraintsOnReferences(t *testing.T) {
	s := compile(t)
	// url's pattern is not checked while it is a reference; db.password is.
	c := cfg(t, `
url: {$kv: ssm://url}
db: {password: {$secret: aws-sm://pw}}
`)
	if errs := s.Validate(c); len(errs) > 0 {
		t.Fatalf("unexpected: %s", joined(errs))
	}
	// A reference always yields a string, so an integer field rejects one.
	c = cfg(t, `
url: https://x
port: {$kv: ssm://port}
db: {}
`)
	if got := joined(s.Validate(c)); !strings.Contains(got, "config.port") {
		t.Fatalf("want a type error on config.port, got %q", got)
	}
	// Literals are checked normally.
	c = cfg(t, `
url: ftp://x
db: {}
`)
	if got := joined(s.Validate(c)); !strings.Contains(got, "config.url") {
		t.Fatalf("want a pattern error on config.url, got %q", got)
	}
	if got := joined(s.Validate(cfg(t, `db: {}`))); !strings.Contains(got, "url") {
		t.Fatalf("want a required error, got %q", got)
	}
}

func TestInvalidSchemas(t *testing.T) {
	for name, src := range map[string]string{
		"bad type":  "type: objekt",
		"bad x-ref": "properties: {a: {x-ref: [vault]}}",
		"not a map": "- a",
	} {
		if _, err := Compile(name, []byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

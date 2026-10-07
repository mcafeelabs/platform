// Package manifest defines service.yaml: what a service provides, what it
// consumes, and how its resources behave in a sandbox.
package manifest

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/mcafeelabs/platform/pkg/capabilities"
	"sigs.k8s.io/yaml"
)

// APIVersion is the only manifest version this renderer understands.
const APIVersion = "platform/v1"

// Sandbox modes for a provided resource.
const (
	SandboxShare   = "share"
	SandboxIsolate = "isolate"
)

// Output is one resource the service creates.
type Output struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Sandbox string `json:"sandbox,omitempty"`
}

// Consume is an edge to a named output of another service.
type Consume struct {
	Service string `json:"service"`
	Output  string `json:"output"`
}

// After is an ordering-only edge.
type After struct {
	Service string `json:"service"`
}

// Config points at the service's config schema.
type Config struct {
	Schema string `json:"schema"`
}

// Manifest is service.yaml.
type Manifest struct {
	APIVersion   string    `json:"apiVersion"`
	Name         string    `json:"name"`
	Owner        string    `json:"owner"`
	ChartProfile string    `json:"chartProfile"`
	Provides     []Output  `json:"provides,omitempty"`
	Consumes     []Consume `json:"consumes,omitempty"`
	DeployAfter  []After   `json:"deployAfter,omitempty"`
	Config       Config    `json:"config"`

	// Path is where the manifest was loaded from (not serialized).
	Path string `json:"-"`
}

var dnsLabel = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// outputName allows dots for NATS subjects such as orders.created.
var outputName = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)

// Load reads and parses a service.yaml (strict: unknown fields are errors).
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Path = path
	return &m, nil
}

// Output returns the named output, if declared.
func (m *Manifest) Output(name string) (Output, bool) {
	for _, o := range m.Provides {
		if o.Name == name {
			return o, true
		}
	}
	return Output{}, false
}

// SandboxMode returns the effective sandbox mode of an output.
func (o Output) SandboxMode(caps *capabilities.Capabilities) string {
	if o.Sandbox != "" {
		return o.Sandbox
	}
	if caps != nil {
		if k, ok := caps.Kinds[o.Kind]; ok && k.DefaultSandbox != "" {
			return k.DefaultSandbox
		}
	}
	return SandboxShare
}

// Dependencies returns the services this one must deploy after, sorted and
// de-duplicated. Self-references are ignored.
func (m *Manifest) Dependencies() []string {
	seen := map[string]bool{}
	for _, c := range m.Consumes {
		if c.Service != m.Name {
			seen[c.Service] = true
		}
	}
	for _, a := range m.DeployAfter {
		if a.Service != m.Name {
			seen[a.Service] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Validate checks a manifest on its own: field formats, unique output names,
// and kinds and profile the chart supports. Cross-service checks live in the
// registry package.
func (m *Manifest) Validate(caps *capabilities.Capabilities) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if m.APIVersion != APIVersion {
		add("apiVersion must be %q, got %q", APIVersion, m.APIVersion)
	}
	if !dnsLabel.MatchString(m.Name) || len(m.Name) > 40 {
		add("name %q must be a DNS label of at most 40 characters", m.Name)
	}
	if strings.TrimSpace(m.Owner) == "" {
		add("owner is required")
	}
	if m.ChartProfile == "" {
		add("chartProfile is required")
	} else if caps != nil && !caps.HasProfile(m.ChartProfile) {
		add("chartProfile %q is not supported by the chart (supported: %s)", m.ChartProfile, strings.Join(caps.Profiles, ", "))
	}
	if m.Config.Schema == "" {
		add("config.schema is required")
	}

	names := map[string]bool{}
	for i, o := range m.Provides {
		switch {
		case o.Name == "":
			add("provides[%d]: name is required", i)
		case !outputName.MatchString(o.Name):
			add("provides[%d]: name %q must be lowercase alphanumerics, '-' or '.'", i, o.Name)
		case names[o.Name]:
			add("provides[%d]: output name %q is declared twice", i, o.Name)
		}
		names[o.Name] = true
		if caps != nil {
			if _, ok := caps.Kinds[o.Kind]; !ok {
				add("provides[%d] %s: kind %q is not supported by the chart (supported: %s)", i, o.Name, o.Kind, strings.Join(caps.KindNames(), ", "))
			}
		}
		switch o.Sandbox {
		case "", SandboxShare, SandboxIsolate:
		default:
			add("provides[%d] %s: sandbox must be %q or %q, got %q", i, o.Name, SandboxShare, SandboxIsolate, o.Sandbox)
		}
	}
	for i, c := range m.Consumes {
		if c.Service == "" || c.Output == "" {
			add("consumes[%d]: service and output are required", i)
		}
	}
	for i, a := range m.DeployAfter {
		if a.Service == "" {
			add("deployAfter[%d]: service is required", i)
		}
	}
	return errs
}

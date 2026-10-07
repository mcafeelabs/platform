// Package capabilities describes what the central Helm chart supports: the
// chart profiles a service may select and the resource kinds it may provide.
// The chart repo publishes capabilities.yaml; the services repo vendors a copy
// so the renderer can check manifests without fetching the chart.
package capabilities

import (
	"fmt"
	"os"
	"sort"

	"sigs.k8s.io/yaml"
)

// OutputMode says how consumers find a provided resource.
type OutputMode string

const (
	// Deterministic outputs are derived from naming conventions and rendered
	// as literals.
	Deterministic OutputMode = "deterministic"
	// Dynamic outputs only exist after provisioning; they are published to the
	// outputs store and consumed through an ExternalSecret.
	Dynamic OutputMode = "dynamic"
)

// Kind is a resource kind a service can declare under provides.
type Kind struct {
	Output OutputMode `json:"output"`
	// Value is a Go text/template for deterministic outputs. Fields:
	// .Env .Service .Output .Namespace .Name (the declared output name).
	Value string `json:"value,omitempty"`
	// Keys lists the properties a dynamic output publishes (e.g. host, password).
	Keys []string `json:"keys,omitempty"`
	// DefaultSandbox is the sandbox mode when the manifest leaves it unset.
	DefaultSandbox string `json:"defaultSandbox,omitempty"`
}

// Capabilities is the content of capabilities.yaml.
type Capabilities struct {
	APIVersion   string          `json:"apiVersion"`
	ChartVersion string          `json:"chartVersion,omitempty"`
	Profiles     []string        `json:"profiles"`
	Kinds        map[string]Kind `json:"kinds"`
}

// Load reads capabilities.yaml.
func Load(path string) (*Capabilities, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Capabilities
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Profiles) == 0 || len(c.Kinds) == 0 {
		return nil, fmt.Errorf("%s: profiles and kinds must not be empty", path)
	}
	for name, k := range c.Kinds {
		switch k.Output {
		case Deterministic:
			if k.Value == "" {
				return nil, fmt.Errorf("%s: kind %s: deterministic kinds need a value template", path, name)
			}
		case Dynamic:
			if len(k.Keys) == 0 {
				return nil, fmt.Errorf("%s: kind %s: dynamic kinds need keys", path, name)
			}
		default:
			return nil, fmt.Errorf("%s: kind %s: output must be deterministic or dynamic", path, name)
		}
	}
	return &c, nil
}

// HasProfile reports whether the chart supports a chartProfile.
func (c *Capabilities) HasProfile(p string) bool {
	for _, x := range c.Profiles {
		if x == p {
			return true
		}
	}
	return false
}

// KindNames returns the supported kinds, sorted.
func (c *Capabilities) KindNames() []string {
	out := make([]string, 0, len(c.Kinds))
	for k := range c.Kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

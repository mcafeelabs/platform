// Package registry loads the services repo: registry.yaml, the config layers,
// every service's manifest and schema, and sandbox definitions. Validate runs
// the publish-time checks across all of them.
package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mcafeelabs/platform/pkg/capabilities"
	"github.com/mcafeelabs/platform/pkg/graph"
	"github.com/mcafeelabs/platform/pkg/manifest"
	"github.com/mcafeelabs/platform/pkg/schema"
	"sigs.k8s.io/yaml"
)

// Config is registry.yaml at the root of the services repo.
type Config struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Git is where Argo CD and Kargo read this repo.
	Git struct {
		RepoURL  string `json:"repoURL"`
		Revision string `json:"revision"`
	} `json:"git"`
	// Chart is the central Helm chart every service deploys with.
	Chart struct {
		RepoURL  string `json:"repoURL"`
		Path     string `json:"path"`
		Revision string `json:"revision"`
	} `json:"chart"`
	// Images.Registry prefixes service image names (registry/<svc>).
	Images struct {
		Registry string `json:"registry"`
	} `json:"images"`
	ArgoCD struct {
		Namespace string `json:"namespace"`
		Project   string `json:"project"`
	} `json:"argocd"`
	Kargo struct {
		Project string `json:"project"`
		// ImageConstraint is the semver constraint Warehouses subscribe with.
		ImageConstraint string `json:"imageConstraint,omitempty"`
	} `json:"kargo"`
	// Tools.Image is the svcreg image (kv-sync and the query service).
	Tools struct {
		Image string `json:"image"`
	} `json:"tools"`
	// Capabilities is the vendored copy of the chart's capabilities.yaml.
	Capabilities string `json:"capabilities"`
}

// Service is one services/<svc>/ directory.
type Service struct {
	Dir      string
	Manifest *manifest.Manifest
	Schema   *schema.Schema
	// Values is layer 3, services/<svc>/values.yaml.
	Values map[string]any
	// EnvValues is layer 4, services/<svc>/environments/<env>/values.yaml.
	EnvValues map[string]map[string]any
}

// SandboxService is one service a sandbox runs.
type SandboxService struct {
	Name   string         `json:"name"`
	Image  string         `json:"image,omitempty"`
	Values map[string]any `json:"values,omitempty"`
}

// Sandbox is sandboxes/<name>.yaml.
type Sandbox struct {
	Name     string           `json:"name"`
	Owner    string           `json:"owner"`
	Baseline string           `json:"baseline"`
	TTL      string           `json:"ttl"`
	Created  string           `json:"created,omitempty"`
	Services []SandboxService `json:"services"`

	Path string `json:"-"`
}

// Service returns the sandbox's entry for svc.
func (s *Sandbox) Service(svc string) (SandboxService, bool) {
	for _, x := range s.Services {
		if x.Name == svc {
			return x, true
		}
	}
	return SandboxService{}, false
}

// Overrides reports whether the sandbox runs its own copy of svc.
func (s *Sandbox) Overrides(svc string) bool {
	_, ok := s.Service(svc)
	return ok
}

// ServiceNames returns the services the sandbox runs, sorted.
func (s *Sandbox) ServiceNames() []string {
	out := make([]string, 0, len(s.Services))
	for _, x := range s.Services {
		out = append(out, x.Name)
	}
	sort.Strings(out)
	return out
}

// Registry is the loaded services repo.
type Registry struct {
	Root      string
	Config    Config
	Caps      *capabilities.Capabilities
	Global    map[string]any
	Envs      map[string]map[string]any
	Services  map[string]*Service
	Sandboxes map[string]*Sandbox
}

// Errors collects load and validation problems.
type Errors []error

func (e Errors) Error() string {
	msgs := make([]string, len(e))
	for i, x := range e {
		msgs[i] = "  - " + x.Error()
	}
	return fmt.Sprintf("%d problem(s):\n%s", len(e), strings.Join(msgs, "\n"))
}

func readYAML(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readLayer(path string) (map[string]any, error) {
	m := map[string]any{}
	if err := readYAML(path, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func dirs(path string) ([]string, error) {
	ents, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Load reads the services repo at root. It returns Errors for anything it
// could not read; call Validate for the cross-service checks.
func Load(root string) (*Registry, error) {
	r := &Registry{
		Root:      root,
		Envs:      map[string]map[string]any{},
		Services:  map[string]*Service{},
		Sandboxes: map[string]*Sandbox{},
	}
	var errs Errors
	fail := func(err error) { errs = append(errs, err) }

	cfgPath := filepath.Join(root, "registry.yaml")
	if b, err := os.ReadFile(cfgPath); err != nil {
		return nil, err
	} else if err := yaml.UnmarshalStrict(b, &r.Config); err != nil {
		return nil, fmt.Errorf("%s: %w", cfgPath, err)
	}
	if r.Config.Capabilities == "" {
		r.Config.Capabilities = "platform/capabilities.yaml"
	}
	if r.Config.ArgoCD.Namespace == "" {
		r.Config.ArgoCD.Namespace = "argocd"
	}
	if r.Config.ArgoCD.Project == "" {
		r.Config.ArgoCD.Project = "default"
	}
	caps, err := capabilities.Load(filepath.Join(root, r.Config.Capabilities))
	if err != nil {
		return nil, err
	}
	r.Caps = caps

	if r.Global, err = readLayer(filepath.Join(root, "_global", "values.yaml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(err)
	}
	if r.Global == nil {
		r.Global = map[string]any{}
	}

	envs, err := dirs(filepath.Join(root, "environments"))
	if err != nil {
		fail(err)
	}
	for _, env := range envs {
		l, err := readLayer(filepath.Join(root, "environments", env, "values.yaml"))
		if err != nil {
			fail(err)
			continue
		}
		r.Envs[env] = l
	}

	svcs, err := dirs(filepath.Join(root, "services"))
	if err != nil {
		fail(err)
	}
	for _, name := range svcs {
		dir := filepath.Join(root, "services", name)
		s := &Service{Dir: dir, EnvValues: map[string]map[string]any{}}
		m, err := manifest.Load(filepath.Join(dir, "service.yaml"))
		if err != nil {
			fail(err)
			continue
		}
		s.Manifest = m
		if m.Config.Schema != "" {
			sch, err := schema.Load(filepath.Join(dir, filepath.Clean(m.Config.Schema)))
			if err != nil {
				fail(err)
			}
			s.Schema = sch
		}
		if s.Values, err = readLayer(filepath.Join(dir, "values.yaml")); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fail(err)
			}
			s.Values = map[string]any{}
		}
		senvs, err := dirs(filepath.Join(dir, "environments"))
		if err != nil {
			fail(err)
		}
		for _, env := range senvs {
			l, err := readLayer(filepath.Join(dir, "environments", env, "values.yaml"))
			if err != nil {
				fail(err)
				continue
			}
			s.EnvValues[env] = l
		}
		r.Services[name] = s
	}

	sbxFiles, _ := filepath.Glob(filepath.Join(root, "sandboxes", "*.yaml"))
	sort.Strings(sbxFiles)
	for _, f := range sbxFiles {
		var sb Sandbox
		b, err := os.ReadFile(f)
		if err != nil {
			fail(err)
			continue
		}
		if err := yaml.UnmarshalStrict(b, &sb); err != nil {
			fail(fmt.Errorf("%s: %w", f, err))
			continue
		}
		sb.Path = f
		if want := strings.TrimSuffix(filepath.Base(f), ".yaml"); sb.Name != want {
			fail(fmt.Errorf("%s: name %q must match the file name %q", f, sb.Name, want))
			continue
		}
		r.Sandboxes[sb.Name] = &sb
	}

	if len(errs) > 0 {
		return r, errs
	}
	return r, nil
}

// EnvNames returns the environments, sorted.
func (r *Registry) EnvNames() []string {
	out := make([]string, 0, len(r.Envs))
	for e := range r.Envs {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// ServiceNames returns the services, sorted.
func (r *Registry) ServiceNames() []string {
	out := make([]string, 0, len(r.Services))
	for s := range r.Services {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SandboxNames returns the sandboxes, sorted.
func (r *Registry) SandboxNames() []string {
	out := make([]string, 0, len(r.Sandboxes))
	for s := range r.Sandboxes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Graph builds the deployment graph. Unknown targets are skipped; Validate
// reports them.
func (r *Registry) Graph() *graph.Graph {
	deps := map[string][]string{}
	for name, s := range r.Services {
		var ds []string
		for _, d := range s.Manifest.Dependencies() {
			if _, ok := r.Services[d]; ok {
				ds = append(ds, d)
			}
		}
		deps[name] = ds
	}
	return graph.New(deps)
}

var sandboxName = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// Validate runs the publish-time checks: manifests, references between
// services, an acyclic graph, layer files for known environments, and
// sandbox definitions.
func (r *Registry) Validate() error {
	var errs Errors
	add := func(where string, err error) { errs = append(errs, fmt.Errorf("%s: %w", where, err)) }

	if r.Config.APIVersion != manifest.APIVersion || r.Config.Kind != "Registry" {
		add("registry.yaml", fmt.Errorf("want apiVersion %s and kind Registry", manifest.APIVersion))
	}
	if r.Config.Git.RepoURL == "" || r.Config.Chart.RepoURL == "" || r.Config.Images.Registry == "" {
		add("registry.yaml", errors.New("git.repoURL, chart.repoURL and images.registry are required"))
	}
	if len(r.Envs) == 0 {
		add("environments/", errors.New("at least one environment is required"))
	}

	for _, name := range r.ServiceNames() {
		s := r.Services[name]
		m := s.Manifest
		where := filepath.Join("services", name, "service.yaml")
		if m.Name != name {
			add(where, fmt.Errorf("name %q must match its directory %q", m.Name, name))
		}
		for _, err := range m.Validate(r.Caps) {
			add(where, err)
		}
		if s.Schema == nil && m.Config.Schema != "" {
			add(where, fmt.Errorf("schema %s did not load", m.Config.Schema))
		}
		for _, c := range m.Consumes {
			t, ok := r.Services[c.Service]
			if !ok {
				add(where, fmt.Errorf("consumes %s/%s: service %q does not exist", c.Service, c.Output, c.Service))
				continue
			}
			if _, ok := t.Manifest.Output(c.Output); !ok {
				add(where, fmt.Errorf("consumes %s/%s: %s does not provide %q", c.Service, c.Output, c.Service, c.Output))
			}
		}
		for _, a := range m.DeployAfter {
			if _, ok := r.Services[a.Service]; !ok {
				add(where, fmt.Errorf("deployAfter %s: service does not exist", a.Service))
			}
		}
		for env := range s.EnvValues {
			if _, ok := r.Envs[env]; !ok {
				add(filepath.Join("services", name, "environments", env), fmt.Errorf("environment %q does not exist", env))
			}
		}
	}

	if c := r.Graph().Cycle(); c != nil {
		add("services/", c)
	}

	for _, name := range r.SandboxNames() {
		sb := r.Sandboxes[name]
		where := filepath.Join("sandboxes", name+".yaml")
		if !sandboxName.MatchString(name) || len(name) > 30 {
			add(where, fmt.Errorf("name %q must be a DNS label of at most 30 characters", name))
		}
		if sb.Owner == "" {
			add(where, errors.New("owner is required"))
		}
		if _, ok := r.Envs[sb.Baseline]; !ok {
			add(where, fmt.Errorf("baseline environment %q does not exist", sb.Baseline))
		}
		if _, err := ParseTTL(sb.TTL); err != nil {
			add(where, err)
		}
		if sb.Created != "" {
			if _, err := time.Parse(time.RFC3339, sb.Created); err != nil {
				add(where, fmt.Errorf("created must be RFC 3339: %w", err))
			}
		}
		if len(sb.Services) == 0 {
			add(where, errors.New("services must list at least one service"))
		}
		seen := map[string]bool{}
		for _, s := range sb.Services {
			if _, ok := r.Services[s.Name]; !ok {
				add(where, fmt.Errorf("service %q does not exist", s.Name))
			}
			if seen[s.Name] {
				add(where, fmt.Errorf("service %q is listed twice", s.Name))
			}
			seen[s.Name] = true
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// ParseTTL parses a sandbox TTL such as 72h or 7d.
func ParseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("ttl is required")
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%d", &n); err != nil || n <= 0 {
			return 0, fmt.Errorf("ttl %q: want a duration like 72h or 3d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("ttl %q: want a duration like 72h or 3d", s)
	}
	return d, nil
}

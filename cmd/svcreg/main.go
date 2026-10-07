// Command svcreg validates the services registry, renders config, generates
// the Argo CD and Kargo tree, and answers questions about the graph.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mcafeelabs/platform/pkg/capabilities"
	"github.com/mcafeelabs/platform/pkg/generate"
	"github.com/mcafeelabs/platform/pkg/manifest"
	"github.com/mcafeelabs/platform/pkg/registry"
	"github.com/mcafeelabs/platform/pkg/render"
	"github.com/mcafeelabs/platform/pkg/schema"
	"sigs.k8s.io/yaml"
)

var version = "dev"

const usage = `svcreg: service registry renderer and generator

Usage:
  svcreg validate        [--root DIR]
  svcreg graph           [--root DIR] [--format text|dot|json]
  svcreg dependents SVC  [--root DIR]
  svcreg render          [--root DIR] (--env ENV | --sandbox NAME) [--service SVC]
  svcreg effective-config SVC ENV [--root DIR] [--sandbox NAME] [--layers]
  svcreg generate        [--root DIR] [--check] [overrides]
  svcreg sandbox-expire  [--root DIR] [--dry-run]
  svcreg lint-service    [--dir DIR] --capabilities FILE
  svcreg serve           [--root DIR] [--addr :8080]
  svcreg kv-sync         --nats URL --index FILE
  svcreg version

generate overrides (for localdev and CI; never committed):
  --git-url URL --git-revision REV --chart-url URL --chart-revision REV
  --image-registry REG --tools-image IMAGE
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "validate":
		err = cmdValidate(args)
	case "graph":
		err = cmdGraph(args)
	case "dependents":
		err = cmdDependents(args)
	case "render":
		err = cmdRender(args)
	case "effective-config":
		err = cmdEffectiveConfig(args)
	case "generate":
		err = cmdGenerate(args)
	case "sandbox-expire":
		err = cmdSandboxExpire(args)
	case "lint-service":
		err = cmdLintService(args)
	case "serve":
		err = cmdServe(args)
	case "kv-sync":
		err = cmdKVSync(args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func load(root string) (*registry.Registry, error) {
	reg, err := registry.Load(root)
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return reg, nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	reg, err := load(*root)
	if err != nil {
		return err
	}
	rd, err := render.New(reg)
	if err != nil {
		return err
	}
	var errs render.Errors
	for _, env := range reg.EnvNames() {
		if _, err := rd.Env(env); err != nil {
			errs = append(errs, err)
		}
	}
	for _, sb := range reg.SandboxNames() {
		if _, err := rd.Sandbox(sb); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs
	}
	fmt.Printf("ok: %d services, %d environments, %d sandboxes\n", len(reg.Services), len(reg.Envs), len(reg.Sandboxes))
	return nil
}

func cmdGraph(args []string) error {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	format := fs.String("format", "text", "text, dot or json")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	reg, err := load(*root)
	if err != nil {
		return err
	}
	g := reg.Graph()
	waves, err := g.Waves()
	if err != nil {
		return err
	}
	switch *format {
	case "dot":
		fmt.Print(g.DOT())
	case "json":
		type node struct {
			Service      string   `json:"service"`
			Wave         int      `json:"wave"`
			Dependencies []string `json:"dependencies"`
		}
		var out []node
		for _, n := range g.Nodes() {
			deps := g.Dependencies(n)
			if deps == nil {
				deps = []string{}
			}
			out = append(out, node{n, waves[n], deps})
		}
		return writeJSON(os.Stdout, out)
	default:
		order, _ := g.Order()
		for _, n := range order {
			deps := g.Dependencies(n)
			fmt.Printf("wave %d  %-20s %s\n", waves[n], n, strings.Join(deps, ", "))
		}
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdDependents(args []string) error {
	fs := flag.NewFlagSet("dependents", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: svcreg dependents SVC")
	}
	reg, err := load(*root)
	if err != nil {
		return err
	}
	if _, ok := reg.Services[pos[0]]; !ok {
		return fmt.Errorf("unknown service %q", pos[0])
	}
	for _, d := range reg.Graph().Dependents(pos[0]) {
		fmt.Println(d)
	}
	return nil
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	env := fs.String("env", "", "environment")
	sbx := fs.String("sandbox", "", "sandbox")
	svc := fs.String("service", "", "only this service")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if (*env == "") == (*sbx == "") {
		return errors.New("pass exactly one of --env or --sandbox")
	}
	reg, err := load(*root)
	if err != nil {
		return err
	}
	rd, err := render.New(reg)
	if err != nil {
		return err
	}
	var results []render.Result
	if *env != "" {
		results, err = rd.Env(*env)
	} else {
		results, err = rd.Sandbox(*sbx)
	}
	if err != nil {
		return err
	}
	first := true
	for _, r := range results {
		if *svc != "" && r.Service != *svc {
			continue
		}
		b, err := r.YAML()
		if err != nil {
			return err
		}
		if !first {
			fmt.Println("---")
		}
		first = false
		fmt.Printf("# %s\n%s", r.Path(), b)
	}
	return nil
}

func cmdEffectiveConfig(args []string) error {
	fs := flag.NewFlagSet("effective-config", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	sbx := fs.String("sandbox", "", "render on top of this sandbox")
	layers := fs.Bool("layers", false, "print the merged layers before resolution")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: svcreg effective-config SVC ENV")
	}
	reg, err := load(*root)
	if err != nil {
		return err
	}
	b, err := effectiveConfig(reg, pos[0], pos[1], *sbx, *layers)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func effectiveConfig(reg *registry.Registry, svc, env, sbx string, layers bool) ([]byte, error) {
	rd, err := render.New(reg)
	if err != nil {
		return nil, err
	}
	t := render.Target{Env: env}
	if sbx != "" {
		sb, ok := reg.Sandboxes[sbx]
		if !ok {
			return nil, fmt.Errorf("unknown sandbox %q", sbx)
		}
		if !sb.Overrides(svc) {
			return nil, fmt.Errorf("sandbox %s does not run %s", sbx, svc)
		}
		t = render.Target{Env: sb.Baseline, Sandbox: sb}
	}
	if layers {
		m, err := rd.Layers(svc, t)
		if err != nil {
			return nil, err
		}
		return yaml.Marshal(m["config"])
	}
	r, err := rd.Service(svc, t)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(r.Values["config"])
}

func gitCreated(root string) func(string) (time.Time, bool) {
	return func(path string) (time.Time, bool) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return time.Time{}, false
		}
		out, err := exec.Command("git", "-C", root, "log", "--diff-filter=A", "--format=%cI", "-1", "--", rel).Output()
		if err != nil {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
		return t, err == nil
	}
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	out := fs.String("out", "", "write to this directory instead of --root")
	check := fs.Bool("check", false, "fail if the committed output is out of date")
	gitURL := fs.String("git-url", "", "override registry.yaml git.repoURL")
	gitRev := fs.String("git-revision", "", "override git.revision")
	chartURL := fs.String("chart-url", "", "override chart.repoURL")
	chartRev := fs.String("chart-revision", "", "override chart.revision")
	imageReg := fs.String("image-registry", "", "override images.registry")
	toolsImage := fs.String("tools-image", "", "override tools.image")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	reg, err := registry.Load(*root)
	if err != nil {
		return err
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&reg.Config.Git.RepoURL, *gitURL)
	set(&reg.Config.Git.Revision, *gitRev)
	set(&reg.Config.Chart.RepoURL, *chartURL)
	set(&reg.Config.Chart.Revision, *chartRev)
	set(&reg.Config.Images.Registry, *imageReg)
	set(&reg.Config.Tools.Image, *toolsImage)

	files, err := generate.Generate(reg, generate.Options{Created: gitCreated(*root)})
	if err != nil {
		return err
	}
	dest := *root
	if *out != "" {
		dest = *out
	}
	if *check {
		diff, err := generate.Diff(dest, files)
		if err != nil {
			return err
		}
		if len(diff) > 0 {
			return fmt.Errorf("generated output is out of date; run svcreg generate:\n  %s", strings.Join(diff, "\n  "))
		}
		fmt.Println("ok: generated output is up to date")
		return nil
	}
	if err := generate.Write(dest, files); err != nil {
		return err
	}
	fmt.Printf("wrote %d files\n%s", len(files), generate.Summary(files))
	return nil
}

func cmdSandboxExpire(args []string) error {
	fs := flag.NewFlagSet("sandbox-expire", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo root")
	dry := fs.Bool("dry-run", false, "only list expired sandboxes")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	reg, err := registry.Load(*root)
	if err != nil {
		return err
	}
	now := time.Now()
	created := gitCreated(*root)
	var expired []string
	for _, name := range reg.SandboxNames() {
		sb := reg.Sandboxes[name]
		if ok, at := generate.Expired(sb, now, created); ok {
			fmt.Printf("expired: %s (owner %s, expired %s)\n", name, sb.Owner, at.UTC().Format(time.RFC3339))
			expired = append(expired, sb.Path)
		}
	}
	if *dry {
		return nil
	}
	for _, p := range expired {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// cmdLintService checks a service repo before CI publishes it: the manifest on
// its own and the schema. Cross-service checks run in the services repo.
func cmdLintService(args []string) error {
	fs := flag.NewFlagSet("lint-service", flag.ContinueOnError)
	dir := fs.String("dir", ".", "service repo root (holds service.yaml)")
	capsPath := fs.String("capabilities", "", "the chart's capabilities.yaml")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	var caps *capabilities.Capabilities
	if *capsPath != "" {
		c, err := capabilities.Load(*capsPath)
		if err != nil {
			return err
		}
		caps = c
	}
	m, err := manifest.Load(filepath.Join(*dir, "service.yaml"))
	if err != nil {
		return err
	}
	var errs registry.Errors
	errs = append(errs, m.Validate(caps)...)
	if m.Config.Schema != "" {
		if _, err := schema.Load(filepath.Join(*dir, filepath.Clean(m.Config.Schema))); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs
	}
	deps := m.Dependencies()
	sort.Strings(deps)
	fmt.Printf("ok: %s (profile %s, provides %d, depends on: %s)\n", m.Name, m.ChartProfile, len(m.Provides), strings.Join(deps, ", "))
	return nil
}

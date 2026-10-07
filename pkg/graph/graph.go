// Package graph builds the deployment DAG from consumes and deployAfter edges,
// detects cycles and assigns each service a wave: its depth, the longest path
// from a service with no dependencies.
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Graph maps a service to the services it deploys after.
type Graph struct {
	deps map[string][]string
}

// New builds a graph. deps[svc] lists svc's dependencies; every dependency
// must itself be a key (the registry checks that before building).
func New(deps map[string][]string) *Graph {
	g := &Graph{deps: map[string][]string{}}
	for s, ds := range deps {
		cp := append([]string(nil), ds...)
		sort.Strings(cp)
		g.deps[s] = cp
	}
	return g
}

// Nodes returns all services, sorted.
func (g *Graph) Nodes() []string {
	out := make([]string, 0, len(g.deps))
	for s := range g.deps {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Dependencies returns the direct dependencies of svc.
func (g *Graph) Dependencies(svc string) []string { return g.deps[svc] }

// CycleError names a dependency cycle, each service depending on the next:
// a -> b -> c -> a.
type CycleError struct{ Cycle []string }

func (e *CycleError) Error() string {
	return "deployment graph has a cycle: " + strings.Join(e.Cycle, " -> ") + " (each depends on the next)"
}

// Cycle returns the first cycle found (deterministically), or nil.
func (g *Graph) Cycle() *CycleError {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		for _, d := range g.deps[n] {
			switch color[d] {
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				found = append(append([]string(nil), stack[i:]...), d)
				return true
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, n := range g.Nodes() {
		if color[n] == white && visit(n) {
			return &CycleError{Cycle: found}
		}
	}
	return nil
}

// Waves returns each service's depth. It fails on a cycle.
func (g *Graph) Waves() (map[string]int, error) {
	if c := g.Cycle(); c != nil {
		return nil, c
	}
	depth := map[string]int{}
	var walk func(string) int
	walk = func(n string) int {
		if d, ok := depth[n]; ok {
			return d
		}
		d := 0
		for _, dep := range g.deps[n] {
			if x := walk(dep) + 1; x > d {
				d = x
			}
		}
		depth[n] = d
		return d
	}
	for _, n := range g.Nodes() {
		walk(n)
	}
	return depth, nil
}

// Order returns services sorted by wave, then name.
func (g *Graph) Order() ([]string, error) {
	w, err := g.Waves()
	if err != nil {
		return nil, err
	}
	out := g.Nodes()
	sort.SliceStable(out, func(i, j int) bool { return w[out[i]] < w[out[j]] })
	return out, nil
}

// Dependents returns every service that transitively depends on svc, sorted.
func (g *Graph) Dependents(svc string) []string {
	rev := map[string][]string{}
	for s, ds := range g.deps {
		for _, d := range ds {
			rev[d] = append(rev[d], s)
		}
	}
	seen := map[string]bool{}
	queue := []string{svc}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, s := range rev[n] {
			if !seen[s] {
				seen[s] = true
				queue = append(queue, s)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// DOT renders the graph in Graphviz format, edges pointing at dependencies.
func (g *Graph) DOT() string {
	var b strings.Builder
	b.WriteString("digraph services {\n  rankdir=BT;\n")
	w, _ := g.Waves()
	for _, n := range g.Nodes() {
		if w != nil {
			fmt.Fprintf(&b, "  %q [label=\"%s\\nwave %d\"];\n", n, n, w[n])
		} else {
			fmt.Fprintf(&b, "  %q;\n", n)
		}
	}
	for _, n := range g.Nodes() {
		for _, d := range g.deps[n] {
			fmt.Fprintf(&b, "  %q -> %q;\n", n, d)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

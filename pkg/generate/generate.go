// Package generate writes everything Argo CD and Kargo read from the services
// repo: rendered values per environment and sandbox, one app-of-apps per
// environment with sync waves from the graph, sandbox routing, and Kargo
// Warehouses and Stages.
//
// Generated layout:
//
//	rendered/<env>/<svc>/values.yaml
//	rendered/sbx/<sandbox>/<svc>/values.yaml
//	apps/<env>/                  root app content: namespace, waypoint,
//	                             sandbox index, kv-sync, one Application per service
//	apps/_sandboxes/<sandbox>/   namespace, quota, ReferenceGrant, HTTPRoutes
//	apps/_platform/              ApplicationSets and Kargo resources
package generate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mcafeelabs/platform/pkg/capabilities"
	"github.com/mcafeelabs/platform/pkg/registry"
	"github.com/mcafeelabs/platform/pkg/render"
	"github.com/mcafeelabs/platform/pkg/values"
	"sigs.k8s.io/yaml"
)

// Roots are the directories generate owns. Write replaces them wholesale.
var Roots = []string{"rendered", "apps"}

// Files maps a repo-relative path to its content.
type Files map[string][]byte

// Paths returns the paths, sorted.
func (f Files) Paths() []string {
	out := make([]string, 0, len(f))
	for p := range f {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (f Files) addDocs(path string, docs ...map[string]any) error {
	var b bytes.Buffer
	b.WriteString(render.Header)
	for i, d := range docs {
		if i > 0 {
			b.WriteString("---\n")
		}
		y, err := yaml.Marshal(d)
		if err != nil {
			return err
		}
		b.Write(y)
	}
	f[path] = b.Bytes()
	return nil
}

// envSettings is the platform block of an environment (global + env layer).
type envSettings struct {
	Env       string
	Namespace string
	Delivery  string
	Upstream  string
	Waypoint  bool
	NATSURL   string
	Domain    string
	Sandbox   map[string]any
}

func (g *generator) settings(env string) envSettings {
	m := values.Merge(g.reg.Global, g.reg.Envs[env])
	s := envSettings{
		Env:       env,
		Namespace: values.GetString(m, "platform", "namespace"),
		Delivery:  values.GetString(m, "platform", "delivery"),
		Upstream:  values.GetString(m, "platform", "kargo", "upstream"),
		Waypoint:  true,
		NATSURL:   values.GetString(m, "platform", "nats", "url"),
		Domain:    values.GetString(m, "platform", "domain"),
		Sandbox:   values.GetMap(m, "platform", "sandbox"),
	}
	if s.Namespace == "" {
		s.Namespace = env
	}
	if s.Delivery == "" {
		s.Delivery = "direct"
	}
	if v, ok := values.Get(m, "platform", "waypoint", "enabled"); ok {
		s.Waypoint, _ = v.(bool)
	}
	return s
}

type generator struct {
	reg   *registry.Registry
	rd    *render.Renderer
	waves map[string]int
	files Files
	now   time.Time
}

// Options tune generation.
type Options struct {
	// Now decides which sandboxes have expired; zero means time.Now().
	Now time.Time
	// Created returns when a sandbox file was added (e.g. from git history)
	// for sandboxes without a created field; nil skips TTL checks for them.
	Created func(path string) (time.Time, bool)
}

// Generate renders every environment and sandbox and builds the app tree.
func Generate(reg *registry.Registry, opts Options) (Files, error) {
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	rd, err := render.New(reg)
	if err != nil {
		return nil, err
	}
	waves, _ := reg.Graph().Waves()
	g := &generator{reg: reg, rd: rd, waves: waves, files: Files{}, now: opts.Now}
	if g.now.IsZero() {
		g.now = time.Now()
	}

	var errs render.Errors
	for _, env := range reg.EnvNames() {
		results, err := rd.Env(env)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range results {
			b, err := r.YAML()
			if err != nil {
				return nil, err
			}
			g.files[r.Path()] = b
		}
		if err := g.envApps(env); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range reg.SandboxNames() {
		results, err := rd.Sandbox(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range results {
			b, err := r.YAML()
			if err != nil {
				return nil, err
			}
			g.files[r.Path()] = b
		}
		if err := g.sandboxRouting(reg.Sandboxes[name], opts); err != nil {
			errs = append(errs, err)
		}
	}
	if err := g.platformApps(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return g.files, nil
}

func wave(n int) map[string]any {
	return map[string]any{"argocd.argoproj.io/sync-wave": strconv.Itoa(n)}
}

func syncPolicy() map[string]any {
	return map[string]any{
		"automated":   map[string]any{"prune": true, "selfHeal": true},
		"syncOptions": []any{"CreateNamespace=false", "ServerSideApply=true"},
		"retry": map[string]any{
			"limit":   10,
			"backoff": map[string]any{"duration": "10s", "factor": 2, "maxDuration": "3m"},
		},
	}
}

func (g *generator) envApps(env string) error {
	s := g.settings(env)
	dir := "apps/" + env + "/"
	nsLabels := map[string]any{
		"istio.io/dataplane-mode":    "ambient",
		"platform.mcafeelabs.io/env": env,
	}
	if s.Waypoint {
		nsLabels["istio.io/use-waypoint"] = "waypoint"
	}
	if err := g.files.addDocs(dir+"namespace.yaml", map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": s.Namespace, "labels": nsLabels, "annotations": wave(-3)},
	}); err != nil {
		return err
	}
	if s.Waypoint {
		if err := g.files.addDocs(dir+"waypoint.yaml", map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
			"metadata": map[string]any{
				"name": "waypoint", "namespace": s.Namespace,
				"labels":      map[string]any{"istio.io/waypoint-for": "service"},
				"annotations": wave(-2),
			},
			"spec": map[string]any{
				"gatewayClassName": "istio-waypoint",
				"listeners":        []any{map[string]any{"name": "mesh", "port": 15008, "protocol": "HBONE"}},
			},
		}); err != nil {
			return err
		}
	}
	if s.NATSURL != "" && g.reg.Config.Tools.Image != "" {
		if err := g.kvSync(dir, s); err != nil {
			return err
		}
	}
	for _, svc := range g.reg.ServiceNames() {
		if err := g.files.addDocs(dir+svc+".yaml", g.serviceApp(svc, s)); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) chartSource() map[string]any {
	c := g.reg.Config.Chart
	return map[string]any{
		"repoURL":        c.RepoURL,
		"path":           c.Path,
		"targetRevision": c.Revision,
	}
}

func (g *generator) serviceApp(svc string, s envSettings) map[string]any {
	cfg := g.reg.Config
	chart := g.chartSource()
	valuesRev, valuesFile := cfg.Git.Revision, "$values/rendered/"+s.Env+"/"+svc+"/values.yaml"
	annotations := wave(g.waves[svc])
	if s.Delivery == "kargo" {
		valuesRev, valuesFile = StageBranch(s.Env, svc), "$values/values.yaml"
		annotations["kargo.akuity.io/authorized-stage"] = cfg.Kargo.Project + ":" + StageName(svc, s.Env)
	}
	chart["helm"] = map[string]any{"releaseName": svc, "valueFiles": []any{valuesFile}}
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{
			"name": s.Env + "-" + svc, "namespace": cfg.ArgoCD.Namespace,
			"annotations": annotations,
			"labels": map[string]any{
				"platform.mcafeelabs.io/env":     s.Env,
				"platform.mcafeelabs.io/service": svc,
			},
			"finalizers": []any{"resources-finalizer.argocd.argoproj.io"},
		},
		"spec": map[string]any{
			"project": cfg.ArgoCD.Project,
			"sources": []any{
				chart,
				map[string]any{"repoURL": cfg.Git.RepoURL, "targetRevision": valuesRev, "ref": "values"},
			},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": s.Namespace},
			"syncPolicy":  syncPolicy(),
		},
	}
}

// StageName is the Kargo Stage that promotes svc into env.
func StageName(svc, env string) string { return svc + "-" + env }

// StageBranch is the branch Kargo writes svc's values for env to.
func StageBranch(env, svc string) string { return "stage/" + env + "/" + svc }

// sandboxIndex lists the sandboxes on top of env and the services each runs.
func (g *generator) sandboxIndex(env string) map[string]any {
	idx := map[string]any{}
	for _, name := range g.reg.SandboxNames() {
		sb := g.reg.Sandboxes[name]
		if sb.Baseline != env {
			continue
		}
		svcs := []any{}
		for _, s := range sb.ServiceNames() {
			svcs = append(svcs, s)
		}
		idx[name] = map[string]any{"services": svcs}
	}
	return idx
}

func (g *generator) kvSync(dir string, s envSettings) error {
	idx, err := json.MarshalIndent(g.sandboxIndex(s.Env), "", "  ")
	if err != nil {
		return err
	}
	labels := map[string]any{"app.kubernetes.io/name": "sandbox-kv-sync"}
	return g.files.addDocs(dir+"sandbox-index.yaml",
		map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "sandbox-index", "namespace": s.Namespace, "annotations": wave(-1)},
			"data":     map[string]any{"sandboxes.json": string(idx) + "\n"},
		},
		map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "sandbox-kv-sync", "namespace": s.Namespace, "annotations": wave(-1), "labels": labels},
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{"matchLabels": labels},
				"template": map[string]any{
					"metadata": map[string]any{"labels": labels},
					"spec": map[string]any{
						"containers": []any{map[string]any{
							"name":  "kv-sync",
							"image": g.reg.Config.Tools.Image,
							"args":  []any{"kv-sync", "--nats", s.NATSURL, "--index", "/etc/sandbox-index/sandboxes.json"},
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
								"limits":   map[string]any{"memory": "64Mi"},
							},
							"volumeMounts": []any{map[string]any{"name": "index", "mountPath": "/etc/sandbox-index"}},
						}},
						"volumes": []any{map[string]any{"name": "index", "configMap": map[string]any{"name": "sandbox-index"}}},
					},
				},
			},
		},
	)
}

// Expired reports whether a sandbox's TTL has run out.
func Expired(sb *registry.Sandbox, now time.Time, created func(string) (time.Time, bool)) (bool, time.Time) {
	ttl, err := registry.ParseTTL(sb.TTL)
	if err != nil {
		return false, time.Time{}
	}
	var start time.Time
	if sb.Created != "" {
		start, _ = time.Parse(time.RFC3339, sb.Created)
	} else if created != nil {
		var ok bool
		if start, ok = created(sb.Path); !ok {
			return false, time.Time{}
		}
	} else {
		return false, time.Time{}
	}
	exp := start.Add(ttl)
	return now.After(exp), exp
}

func (g *generator) providesHTTP(svc string) bool {
	for _, o := range g.reg.Services[svc].Manifest.Provides {
		if g.reg.Caps.Kinds[o.Kind].Output == capabilities.Deterministic && o.Kind == "http-endpoint" {
			return true
		}
	}
	return false
}

// BaggageRegex matches a W3C baggage header carrying sandbox=<name> as one of
// its members (RE2, full match as Envoy requires).
func BaggageRegex(name string) string {
	return `^(.*[,;]\s*)?sandbox=` + name + `(\s*[,;].*)?$`
}

func (g *generator) sandboxRouting(sb *registry.Sandbox, opts Options) error {
	s := g.settings(sb.Baseline)
	ns := "sbx-" + sb.Name
	dir := "apps/_sandboxes/" + sb.Name + "/"
	labels := map[string]any{"platform.mcafeelabs.io/sandbox": sb.Name, "platform.mcafeelabs.io/owner": sb.Owner}

	nsAnn := map[string]any{"platform.mcafeelabs.io/ttl": sb.TTL}
	if _, exp := Expired(sb, g.now, opts.Created); !exp.IsZero() {
		nsAnn["platform.mcafeelabs.io/expires"] = exp.UTC().Format(time.RFC3339)
	}
	nsLabels := map[string]any{"istio.io/dataplane-mode": "ambient"}
	for k, v := range labels {
		nsLabels[k] = v
	}
	docs := []map[string]any{{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": ns, "labels": nsLabels, "annotations": nsAnn},
	}}

	quota := values.GetMap(s.Sandbox, "quota")
	if len(quota) == 0 {
		quota = map[string]any{"requests.cpu": "2", "requests.memory": "4Gi", "limits.memory": "8Gi", "pods": "20"}
	}
	docs = append(docs,
		map[string]any{
			"apiVersion": "v1", "kind": "ResourceQuota",
			"metadata": map[string]any{"name": "sandbox", "namespace": ns, "labels": labels},
			"spec":     map[string]any{"hard": quota},
		},
		map[string]any{
			"apiVersion": "v1", "kind": "LimitRange",
			"metadata": map[string]any{"name": "sandbox", "namespace": ns, "labels": labels},
			"spec": map[string]any{"limits": []any{map[string]any{
				"type":           "Container",
				"default":        map[string]any{"memory": "256Mi"},
				"defaultRequest": map[string]any{"cpu": "20m", "memory": "64Mi"},
			}}},
		},
		map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "ReferenceGrant",
			"metadata": map[string]any{"name": "from-baseline", "namespace": ns, "labels": labels},
			"spec": map[string]any{
				"from": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "namespace": s.Namespace}},
				"to":   []any{map[string]any{"group": "", "kind": "Service"}},
			},
		},
	)

	baggage := BaggageRegex(sb.Name)
	for _, svc := range sb.ServiceNames() {
		if !g.providesHTTP(svc) {
			continue
		}
		port := 80
		docs = append(docs, map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
			"metadata": map[string]any{"name": "sbx-" + sb.Name + "-" + svc, "namespace": s.Namespace, "labels": labels},
			"spec": map[string]any{
				"parentRefs": []any{map[string]any{"group": "", "kind": "Service", "name": svc, "port": port}},
				"rules": []any{
					map[string]any{
						"matches": []any{map[string]any{"headers": []any{map[string]any{
							"type": "RegularExpression", "name": "baggage", "value": baggage,
						}}}},
						"backendRefs": []any{map[string]any{"name": svc, "namespace": ns, "port": port}},
					},
					// Every route attached to a Service must also carry the
					// default, or unmatched requests get a 404.
					map[string]any{"backendRefs": []any{map[string]any{"name": svc, "port": port}}},
				},
			},
		})
	}

	gwName := values.GetString(s.Sandbox, "gateway", "name")
	entry := values.GetString(s.Sandbox, "entryService")
	if gwName != "" && entry != "" {
		backend := map[string]any{"name": entry, "port": 80}
		if sb.Overrides(entry) {
			backend["namespace"] = ns
		}
		domain := values.GetString(s.Sandbox, "domain")
		if domain == "" {
			domain = "sbx." + s.Domain
		}
		docs = append(docs, map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
			"metadata": map[string]any{"name": "sbx-" + sb.Name + "-ingress", "namespace": s.Namespace, "labels": labels},
			"spec": map[string]any{
				"parentRefs": []any{map[string]any{
					"name": gwName, "namespace": values.GetString(s.Sandbox, "gateway", "namespace"),
				}},
				"hostnames": []any{sb.Name + "." + domain},
				"rules": []any{map[string]any{
					"filters": []any{map[string]any{
						"type": "RequestHeaderModifier",
						"requestHeaderModifier": map[string]any{
							"set": []any{map[string]any{"name": "baggage", "value": "sandbox=" + sb.Name}},
						},
					}},
					"backendRefs": []any{backend},
				}},
			},
		})
	}
	return g.files.addDocs(dir+"routing.yaml", docs...)
}

func (g *generator) platformApps() error {
	cfg := g.reg.Config
	repo := cfg.Git.RepoURL
	rev := cfg.Git.Revision
	ns := cfg.ArgoCD.Namespace
	app := func(name, path, destNS string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{
				"name":       name,
				"finalizers": []any{"resources-finalizer.argocd.argoproj.io"},
			},
			"spec": map[string]any{
				"project":     cfg.ArgoCD.Project,
				"source":      map[string]any{"repoURL": repo, "targetRevision": rev, "path": path},
				"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": destNS},
				"syncPolicy":  syncPolicy(),
			},
		}
	}

	envsTpl := app("env-{{ .path.basename }}", "{{ .path.path }}", ns)
	envs := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet",
		"metadata": map[string]any{"name": "environments", "namespace": ns},
		"spec": map[string]any{
			"goTemplate":        true,
			"goTemplateOptions": []any{"missingkey=error"},
			"generators": []any{map[string]any{"git": map[string]any{
				"repoURL": repo, "revision": rev,
				"directories": []any{
					map[string]any{"path": "apps/*"},
					map[string]any{"path": "apps/_*", "exclude": true},
				},
			}}},
			"template": envsTpl,
		},
	}

	routingTpl := app("sbx-{{ .path.basename }}-routing", "{{ .path.path }}", ns)
	routing := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet",
		"metadata": map[string]any{"name": "sandbox-routing", "namespace": ns},
		"spec": map[string]any{
			"goTemplate":        true,
			"goTemplateOptions": []any{"missingkey=error"},
			"generators": []any{map[string]any{"git": map[string]any{
				"repoURL": repo, "revision": rev,
				"directories": []any{map[string]any{"path": "apps/_sandboxes/*"}},
			}}},
			"template": routingTpl,
		},
	}

	chart := g.chartSource()
	chart["helm"] = map[string]any{
		"releaseName": "{{ index .path.segments 3 }}",
		"valueFiles":  []any{"$values/{{ .path.path }}/values.yaml"},
	}
	sbxApps := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet",
		"metadata": map[string]any{"name": "sandbox-services", "namespace": ns},
		"spec": map[string]any{
			"goTemplate":        true,
			"goTemplateOptions": []any{"missingkey=error"},
			"generators": []any{map[string]any{"git": map[string]any{
				"repoURL": repo, "revision": rev,
				"files": []any{map[string]any{"path": "rendered/sbx/*/*/values.yaml"}},
			}}},
			"template": map[string]any{
				"metadata": map[string]any{
					"name": "sbx-{{ index .path.segments 2 }}-{{ index .path.segments 3 }}",
					"labels": map[string]any{
						"platform.mcafeelabs.io/sandbox": "{{ index .path.segments 2 }}",
						"platform.mcafeelabs.io/service": "{{ index .path.segments 3 }}",
					},
					"finalizers": []any{"resources-finalizer.argocd.argoproj.io"},
				},
				"spec": map[string]any{
					"project": cfg.ArgoCD.Project,
					"sources": []any{
						chart,
						map[string]any{"repoURL": repo, "targetRevision": rev, "ref": "values"},
					},
					"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "sbx-{{ index .path.segments 2 }}"},
					"syncPolicy":  syncPolicy(),
				},
			},
		},
	}
	if err := g.files.addDocs("apps/_platform/applicationsets.yaml", envs, routing, sbxApps); err != nil {
		return err
	}
	return g.kargo()
}

func (g *generator) kargo() error {
	cfg := g.reg.Config
	var kargoEnvs []envSettings
	for _, env := range g.reg.EnvNames() {
		if s := g.settings(env); s.Delivery == "kargo" {
			kargoEnvs = append(kargoEnvs, s)
		}
	}
	if len(kargoEnvs) == 0 {
		return nil
	}
	project := cfg.Kargo.Project
	constraint := cfg.Kargo.ImageConstraint
	if constraint == "" {
		constraint = ">=0.0.0"
	}
	docs := []map[string]any{
		{
			"apiVersion": "kargo.akuity.io/v1alpha1", "kind": "Project",
			"metadata": map[string]any{"name": project},
		},
	}
	var policies []any
	for _, svc := range g.reg.ServiceNames() {
		var include []any
		for _, s := range kargoEnvs {
			include = append(include, "glob:rendered/"+s.Env+"/"+svc+"/**")
		}
		docs = append(docs, map[string]any{
			"apiVersion": "kargo.akuity.io/v1alpha1", "kind": "Warehouse",
			"metadata": map[string]any{"name": svc, "namespace": project},
			"spec": map[string]any{
				"subscriptions": []any{
					map[string]any{"image": map[string]any{
						"repoURL":                strings.TrimSuffix(cfg.Images.Registry, "/") + "/" + svc,
						"imageSelectionStrategy": "SemVer",
						"constraint":             constraint,
					}},
					map[string]any{"git": map[string]any{
						"repoURL":      cfg.Git.RepoURL,
						"branch":       cfg.Git.Revision,
						"includePaths": include,
					}},
				},
			},
		})
		for _, s := range kargoEnvs {
			stage := StageName(svc, s.Env)
			sources := map[string]any{"direct": true}
			if s.Upstream != "" {
				sources = map[string]any{"stages": []any{StageName(svc, s.Upstream)}}
			}
			image := strings.TrimSuffix(cfg.Images.Registry, "/") + "/" + svc
			docs = append(docs, map[string]any{
				"apiVersion": "kargo.akuity.io/v1alpha1", "kind": "Stage",
				"metadata": map[string]any{"name": stage, "namespace": project},
				"spec": map[string]any{
					"requestedFreight": []any{map[string]any{
						"origin":  map[string]any{"kind": "Warehouse", "name": svc},
						"sources": sources,
					}},
					"promotionTemplate": map[string]any{"spec": map[string]any{
						"vars": []any{
							map[string]any{"name": "gitRepo", "value": cfg.Git.RepoURL},
							map[string]any{"name": "image", "value": image},
						},
						"steps": promotionSteps(svc, s.Env, cfg.Git.Revision, cfg.ArgoCD.Namespace),
					}},
				},
			})
			policies = append(policies, map[string]any{
				"stageSelector":        map[string]any{"name": stage},
				"autoPromotionEnabled": true,
			})
		}
	}
	docs = append(docs, map[string]any{
		"apiVersion": "kargo.akuity.io/v1alpha1", "kind": "ProjectConfig",
		"metadata": map[string]any{"name": project, "namespace": project},
		"spec":     map[string]any{"promotionPolicies": policies},
	})
	return g.files.addDocs("apps/_platform/kargo.yaml", docs...)
}

// promotionSteps copies the freight's rendered values for env into the stage
// branch, sets the freight's image tag, and points Argo CD at the result. Only
// built-in steps.
func promotionSteps(svc, env, mainBranch, argoNS string) []any {
	return []any{
		map[string]any{"uses": "git-clone", "config": map[string]any{
			"repoURL": "${{ vars.gitRepo }}",
			"checkout": []any{
				map[string]any{"commit": "${{ commitFrom(vars.gitRepo).ID }}", "path": "./src"},
				map[string]any{"branch": StageBranch(env, svc), "create": true, "path": "./out"},
			},
		}},
		map[string]any{"uses": "git-clear", "config": map[string]any{"path": "./out"}},
		map[string]any{"uses": "copy", "config": map[string]any{
			"inPath": "./src/rendered/" + env + "/" + svc + "/values.yaml", "outPath": "./out/values.yaml",
		}},
		map[string]any{"uses": "yaml-update", "as": "update-image", "config": map[string]any{
			"path":    "./out/values.yaml",
			"updates": []any{map[string]any{"key": "image.tag", "value": "${{ imageFrom(vars.image).Tag }}"}},
		}},
		map[string]any{"uses": "git-commit", "as": "commit", "config": map[string]any{
			"path":    "./out",
			"message": "promote " + svc + " to " + env + ": ${{ imageFrom(vars.image).Tag }}",
		}},
		map[string]any{"uses": "git-push", "config": map[string]any{"path": "./out"}},
		map[string]any{"uses": "argocd-update", "config": map[string]any{"apps": []any{map[string]any{
			"name": env + "-" + svc, "namespace": argoNS,
			"sources": []any{map[string]any{
				"repoURL":         "${{ vars.gitRepo }}",
				"desiredRevision": "${{ outputs.commit.commit }}",
			}},
		}}}},
	}
}

// Write replaces the generated directories under root with files.
func Write(root string, files Files) error {
	for _, d := range Roots {
		if err := os.RemoveAll(filepath.Join(root, d)); err != nil {
			return err
		}
	}
	for _, p := range files.Paths() {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, files[p], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Diff lists paths under the generated directories whose content differs
// from files (added, changed or stale).
func Diff(root string, files Files) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, d := range Roots {
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if e.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			seen[rel] = true
			want, ok := files[rel]
			if !ok {
				out = append(out, "stale:   "+rel)
				return nil
			}
			got, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				out = append(out, "changed: "+rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, p := range files.Paths() {
		if !seen[p] {
			out = append(out, "missing: "+p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Summary returns a one-line description per environment, for logs.
func Summary(files Files) string {
	counts := map[string]int{}
	for p := range files {
		parts := strings.Split(p, "/")
		if len(parts) >= 3 && parts[0] == "rendered" {
			key := parts[1]
			if key == "sbx" && len(parts) >= 4 {
				key = "sbx/" + parts[2]
			}
			counts[key]++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %d service(s)\n", k, counts[k])
	}
	return b.String()
}

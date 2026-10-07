package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mcafeelabs/platform/pkg/sandbox"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// cmdServe runs the read-only query service over a checkout of the services
// repo (kept fresh by a git-sync sidecar or similar). It reloads the registry
// per request, so it always answers from what is on disk.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := fs.String("root", ".", "services repo checkout")
	addr := fs.String("addr", ":8080", "listen address")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: queryHandler(*root), ReadHeaderTimeout: 5 * time.Second}
	slog.Info("serving registry", "root", *root, "addr", *addr)
	return srv.ListenAndServe()
}

func queryHandler(root string) http.Handler {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, code int, err error) {
		http.Error(w, err.Error(), code)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /graph", func(w http.ResponseWriter, r *http.Request) {
		reg, err := load(root)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		g := reg.Graph()
		waves, err := g.Waves()
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if r.URL.Query().Get("format") == "dot" {
			w.Header().Set("Content-Type", "text/vnd.graphviz")
			fmt.Fprint(w, g.DOT())
			return
		}
		out := map[string]any{}
		for _, n := range g.Nodes() {
			deps := g.Dependencies(n)
			if deps == nil {
				deps = []string{}
			}
			out[n] = map[string]any{"wave": waves[n], "dependencies": deps}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /dependents/{svc}", func(w http.ResponseWriter, r *http.Request) {
		reg, err := load(root)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		svc := r.PathValue("svc")
		if _, ok := reg.Services[svc]; !ok {
			fail(w, http.StatusNotFound, fmt.Errorf("unknown service %q", svc))
			return
		}
		deps := reg.Graph().Dependents(svc)
		if deps == nil {
			deps = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(deps)
	})
	mux.HandleFunc("GET /effective-config/{svc}/{env}", func(w http.ResponseWriter, r *http.Request) {
		reg, err := load(root)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		b, err := effectiveConfig(reg, r.PathValue("svc"), r.PathValue("env"), r.URL.Query().Get("sandbox"), r.URL.Query().Has("layers"))
		if err != nil {
			code := http.StatusUnprocessableEntity
			if strings.Contains(err.Error(), "unknown") {
				code = http.StatusNotFound
			}
			fail(w, code, err)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(b)
	})
	return mux
}

// cmdKVSync keeps the NATS sandboxes bucket in step with the sandbox index
// ConfigMap (mounted as a file; the kubelet refreshes it on change).
func cmdKVSync(args []string) error {
	fs := flag.NewFlagSet("kv-sync", flag.ContinueOnError)
	url := fs.String("nats", nats.DefaultURL, "NATS URL")
	index := fs.String("index", "/etc/sandbox-index/sandboxes.json", "sandbox index file")
	every := fs.Duration("interval", 15*time.Second, "resync interval")
	once := fs.Bool("once", false, "sync once and exit")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nc, err := nats.Connect(*url, nats.MaxReconnects(-1), nats.RetryOnFailedConnect(true))
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	sync := func() error {
		b, err := os.ReadFile(*index)
		if err != nil {
			return err
		}
		var raw map[string]struct {
			Services []string `json:"services"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("%s: %w", *index, err)
		}
		idx := map[string][]string{}
		for n, v := range raw {
			idx[n] = v.Services
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		changed, err := sandbox.SyncBucket(sctx, js, idx)
		if err != nil {
			return err
		}
		if changed > 0 {
			slog.Info("synced sandbox bucket", "sandboxes", len(idx), "changed", changed)
		}
		return nil
	}
	for {
		if err := sync(); err != nil {
			if *once {
				return err
			}
			slog.Warn("kv sync failed", "err", err)
		} else if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-time.After(*every):
		}
	}
}

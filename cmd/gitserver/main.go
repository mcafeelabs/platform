// Command gitserver serves bare git repositories over smart HTTP (pull and
// push) for local clusters, so Argo CD and Kargo can read and write a copy of
// the services repo without touching GitHub. Not for production use.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	root := flag.String("root", "/srv/git", "directory holding <name>.git repositories")
	addr := flag.String("addr", ":8080", "listen address")
	repos := flag.String("init", "", "comma-separated repositories to create if missing")
	user := flag.String("user", os.Getenv("GIT_USER"), "basic auth user for pushes (empty: no auth)")
	pass := flag.String("password", os.Getenv("GIT_PASSWORD"), "basic auth password")
	flag.Parse()

	backend, err := exec.LookPath("git")
	if err != nil {
		slog.Error("git not found", "err", err)
		os.Exit(1)
	}
	for _, r := range strings.Split(*repos, ",") {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		dir := filepath.Join(*root, strings.TrimSuffix(r, ".git")+".git")
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		for _, args := range [][]string{
			{"init", "--bare", "--initial-branch=main", dir},
			{"-C", dir, "config", "http.receivepack", "true"},
		} {
			if out, err := exec.Command(backend, args...).CombinedOutput(); err != nil {
				slog.Error("init failed", "repo", r, "out", string(out))
				os.Exit(1)
			}
		}
		slog.Info("created", "repo", dir)
	}

	h := &cgi.Handler{
		Path: backend,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + *root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		push := r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack")
		if push && *user != "" {
			u, p, ok := r.BasicAuth()
			if !ok || u != *user || p != *pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if push {
			// git http-backend only accepts pushes from an authenticated user.
			h := *h
			h.Env = append(append([]string(nil), h.Env...), "REMOTE_USER="+u(r))
			h.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("serving git", "root", *root, "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func u(r *http.Request) string {
	if name, _, ok := r.BasicAuth(); ok {
		return name
	}
	return "anonymous"
}

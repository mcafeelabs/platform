// Package servicekit is the small runtime every platform service shares: it
// reads the rendered config file, serves HTTP with sandbox propagation,
// calls other services with baggage forwarded, and connects to NATS with the
// sandbox overrides loaded.
package servicekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mcafeelabs/platform/pkg/sandbox"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"sigs.k8s.io/yaml"
)

// Kit holds a service's identity, config and HTTP plumbing.
type Kit struct {
	Name        string
	Environment string
	// Sandbox is the sandbox this copy runs in ("" for the baseline).
	Sandbox string
	Version string
	// Config is the raw config file; use Decode for a typed view.
	Config map[string]any
	Mux    *http.ServeMux
	Client *http.Client
	Log    *slog.Logger
}

// New reads SERVICE_NAME, ENVIRONMENT, SANDBOX_NAME, SERVICE_VERSION and the
// YAML file at CONFIG_FILE (if set), and registers /healthz and /info.
func New() (*Kit, error) {
	sandbox.Install()
	k := &Kit{
		Name:        envOr("SERVICE_NAME", "service"),
		Environment: os.Getenv("ENVIRONMENT"),
		Sandbox:     os.Getenv("SANDBOX_NAME"),
		Version:     envOr("SERVICE_VERSION", "dev"),
		Config:      map[string]any{},
		Mux:         http.NewServeMux(),
		Client:      &http.Client{Transport: sandbox.Transport(nil), Timeout: 10 * time.Second},
	}
	k.Log = slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", k.Name, "sandbox", k.Sandbox)
	if path := os.Getenv("CONFIG_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if err := yaml.Unmarshal(b, &k.Config); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	k.Mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	k.Mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) { k.JSON(w, http.StatusOK, k.Info(r.Context())) })
	return k, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Decode converts the config file into a typed struct (json tags).
func (k *Kit) Decode(into any) error {
	b, err := json.Marshal(k.Config)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// Info describes this copy of the service and the sandbox the request asked
// for. Responses carry it so callers can see which copy served each hop.
func (k *Kit) Info(ctx context.Context) map[string]any {
	return map[string]any{
		"service":        k.Name,
		"version":        k.Version,
		"environment":    k.Environment,
		"sandbox":        k.Sandbox,
		"requestSandbox": sandbox.FromContext(ctx),
	}
}

// JSON writes v with a status code.
func (k *Kit) JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Call sends a request to another service with trace context and baggage
// forwarded, and decodes a JSON response into out.
func (k *Kit) Call(ctx context.Context, method, url string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	resp, err := k.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// Port returns config http.port, or 8080.
func (k *Kit) Port() int {
	var c struct {
		HTTP struct {
			Port int `json:"port"`
		} `json:"http"`
	}
	_ = k.Decode(&c)
	if c.HTTP.Port == 0 {
		return 8080
	}
	return c.HTTP.Port
}

// Handler returns the mux wrapped with baggage extraction.
func (k *Kit) Handler() http.Handler { return sandbox.Middleware(k.Mux) }

// Run serves HTTP until SIGTERM, then shuts down gracefully.
func (k *Kit) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: fmt.Sprintf(":%d", k.Port()), Handler: k.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	k.Log.Info("listening", "addr", srv.Addr, "version", k.Version, "environment", k.Environment)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NATS connects to url and loads the sandbox overrides. It retries until the
// KV bucket exists (kv-sync creates it) or ctx ends.
func (k *Kit) NATS(ctx context.Context, url string) (jetstream.JetStream, *sandbox.Overrides, error) {
	nc, err := nats.Connect(url, nats.Name(k.Name), nats.MaxReconnects(-1), nats.RetryOnFailedConnect(true))
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, nil, err
	}
	o := sandbox.NewOverrides()
	for {
		err := o.Watch(ctx, js)
		if err == nil {
			return js, o, nil
		}
		k.Log.Warn("waiting for sandbox bucket", "err", err)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

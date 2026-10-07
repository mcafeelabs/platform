package servicekit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestKit(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("http: {port: 9090}\nidentity: {url: http://x}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfg)
	t.Setenv("SERVICE_NAME", "payments")
	t.Setenv("SANDBOX_NAME", "sbx-a")
	k, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if k.Port() != 9090 {
		t.Fatalf("port %d", k.Port())
	}
	var c struct {
		Identity struct {
			URL string `json:"url"`
		} `json:"identity"`
	}
	if err := k.Decode(&c); err != nil || c.Identity.URL != "http://x" {
		t.Fatalf("decode %v %+v", err, c)
	}

	// A downstream call forwards the request's baggage.
	down := httptest.NewServer(k.Handler())
	defer down.Close()
	up := http.NewServeMux()
	up.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var info map[string]any
		if err := k.Call(r.Context(), http.MethodGet, down.URL+"/info", nil, &info); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		k.JSON(w, 200, info)
	})
	upSrv := httptest.NewServer(k.Handler())
	defer upSrv.Close()
	k.Mux.HandleFunc("GET /hop", up.ServeHTTP)

	var info map[string]any
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upSrv.URL+"/hop", nil)
	req.Header.Set("baggage", "sandbox=sbx-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := jsonDecode(resp, &info); err != nil {
		t.Fatal(err)
	}
	if info["requestSandbox"] != "sbx-a" || info["sandbox"] != "sbx-a" || info["service"] != "payments" {
		t.Fatalf("info %v", info)
	}
}

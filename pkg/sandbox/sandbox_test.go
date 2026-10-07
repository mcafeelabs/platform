package sandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestHTTPPropagation(t *testing.T) {
	// downstream reports the sandbox it saw.
	downstream := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, FromContext(r.Context()))
	})))
	defer downstream.Close()
	client := &http.Client{Transport: Transport(nil)}
	// upstream calls downstream with the incoming request's context.
	upstream := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, downstream.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(w, resp.Body)
	})))
	defer upstream.Close()

	for header, want := range map[string]string{
		"sandbox=ryan-checkout-v2":          "ryan-checkout-v2",
		"userId=7,sandbox=ryan-checkout-v2": "ryan-checkout-v2",
		"":                                  "",
	} {
		req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
		if header != "" {
			req.Header.Set("baggage", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("baggage %q: downstream saw %q, want %q", header, b, want)
		}
	}
}

func TestShouldProcess(t *testing.T) {
	o := NewOverrides()
	o.Set("sbx-a", []string{"orders"})
	cases := []struct {
		own, msg string
		want     bool
	}{
		{"", "", true},           // baseline, untagged
		{"", "sbx-a", false},     // baseline, sandbox overrides orders
		{"", "sbx-b", true},      // baseline, sandbox that does not override orders
		{"sbx-a", "sbx-a", true}, // sandbox copy, own message
		{"sbx-a", "", false},     // sandbox copy, untagged
		{"sbx-a", "sbx-b", false},
	}
	for _, c := range cases {
		if got := ShouldProcess(o, "orders", c.own, c.msg); got != c.want {
			t.Errorf("own=%q msg=%q: got %v", c.own, c.msg, got)
		}
	}
	o.Set("sbx-a", nil)
	if !ShouldProcess(o, "orders", "", "sbx-a") {
		t.Error("removed sandbox should fall back to baseline")
	}
}

func runServer(t *testing.T) *nats.Conn {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(s.Shutdown)
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestNATSRouting(t *testing.T) {
	nc := runServer(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// kv-sync publishes which services each sandbox overrides.
	if n, err := SyncBucket(ctx, js, map[string][]string{"sbx-a": {"orders"}, "stale": {"x"}}); err != nil || n != 2 {
		t.Fatalf("sync: %d %v", n, err)
	}
	if n, err := SyncBucket(ctx, js, map[string][]string{"sbx-a": {"orders"}}); err != nil || n != 1 {
		t.Fatalf("resync should only delete stale: %d %v", n, err)
	}
	o := NewOverrides()
	if err := o.Watch(ctx, js); err != nil {
		t.Fatal(err)
	}
	if !o.Overrides("sbx-a", "orders") || o.Overrides("stale", "x") {
		t.Fatal("watch did not load the bucket")
	}

	if _, err := EnsureStream(ctx, js, "INVENTORY", []string{"stock.changed"}); err != nil {
		t.Fatal(err)
	}
	type seen struct {
		mu   sync.Mutex
		msgs []string
	}
	baseline, sbx := &seen{}, &seen{}
	record := func(s *seen) Handler {
		return func(ctx context.Context, m jetstream.Msg) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.msgs = append(s.msgs, string(m.Data())+"@"+FromContext(ctx))
			return nil
		}
	}
	for _, c := range []struct {
		own string
		s   *seen
	}{{"", baseline}, {"sbx-a", sbx}} {
		c := c
		cons := Consumer{Service: "orders", Sandbox: c.own, Stream: "INVENTORY", Subjects: []string{"stock.changed"}, Overrides: o}
		go func() { _ = cons.Run(ctx, js, record(c.s)) }()
	}
	// Wait for both consumers to exist before publishing (DeliverNew).
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, e1 := js.Consumer(ctx, "INVENTORY", "orders")
		_, e2 := js.Consumer(ctx, "INVENTORY", "orders-sbx-sbx-a")
		if e1 == nil && e2 == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumers not created: %v %v", e1, e2)
		}
		time.Sleep(50 * time.Millisecond)
	}

	tagged, _ := WithSandbox(ctx, "sbx-a")
	other, _ := WithSandbox(ctx, "sbx-b")
	for _, p := range []struct {
		ctx  context.Context
		data string
	}{{ctx, "plain"}, {tagged, "tagged"}, {other, "other"}} {
		if _, err := Publish(p.ctx, js, "stock.changed", []byte(p.data)); err != nil {
			t.Fatal(err)
		}
	}

	wait := func(s *seen, n int) []string {
		end := time.Now().Add(10 * time.Second)
		for time.Now().Before(end) {
			s.mu.Lock()
			got := append([]string(nil), s.msgs...)
			s.mu.Unlock()
			if len(got) >= n {
				time.Sleep(200 * time.Millisecond) // catch extras
				s.mu.Lock()
				defer s.mu.Unlock()
				return append([]string(nil), s.msgs...)
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	if got := wait(baseline, 2); len(got) != 2 || got[0] != "plain@" || got[1] != "other@sbx-b" {
		t.Errorf("baseline handled %v", got)
	}
	if got := wait(sbx, 1); len(got) != 1 || got[0] != "tagged@sbx-a" {
		t.Errorf("sandbox handled %v", got)
	}

	info, err := js.Consumer(ctx, "INVENTORY", "orders-sbx-sbx-a")
	if err != nil {
		t.Fatal(err)
	}
	if ci, _ := info.Info(ctx); ci.Config.InactiveThreshold != time.Hour {
		t.Errorf("sandbox consumer inactive threshold %v", ci.Config.InactiveThreshold)
	}
}

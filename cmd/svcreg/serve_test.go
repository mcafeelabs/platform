package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mcafeelabs/platform/internal/testutil"
)

func TestQueryHandler(t *testing.T) {
	srv := httptest.NewServer(queryHandler(testutil.Registry(t)))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	cases := []struct {
		path string
		code int
		want string
	}{
		{"/graph", 200, `"orders":{"dependencies":["identity","inventory","payments"],"wave":2}`},
		{"/graph?format=dot", 200, `"payments" -> "identity"`},
		{"/dependents/identity", 200, `["orders","payments"]`},
		{"/dependents/ghost", 404, "unknown service"},
		{"/effective-config/payments/dev", 200, "url: http://identity.dev.svc.cluster.local"},
		{"/effective-config/payments/dev?sandbox=ryan-checkout-v2", 200, "refundsV2: true"},
		{"/effective-config/payments/dev?layers", 200, "$output: identity/api"},
		{"/effective-config/identity/dev?sandbox=ryan-checkout-v2", 422, "does not run identity"},
	}
	for _, c := range cases {
		code, body := get(c.path)
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %q, want %d containing %q", c.path, code, body, c.code, c.want)
		}
	}
}

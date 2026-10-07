// Package sandbox carries a request's sandbox ID across hops. The ID travels
// as the W3C baggage member sandbox=<name>: the gateway sets it, every service
// forwards it on HTTP, gRPC and NATS calls, and Istio waypoints route on it.
//
// HTTP:
//
//	http.Handle("/", sandbox.Middleware(mux))          // extract on the way in
//	client := &http.Client{Transport: sandbox.Transport(nil)} // inject on the way out
//
// NATS: see Publish and Consumer.
package sandbox

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
)

// Member is the baggage member that names the sandbox.
const Member = "sandbox"

// Propagator propagates W3C trace context and baggage.
var Propagator propagation.TextMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{},
)

// Install sets Propagator as the global OTel propagator, so instrumentation
// libraries (otelhttp, otelgrpc) forward baggage too.
func Install() { otel.SetTextMapPropagator(Propagator) }

// FromContext returns the sandbox named in ctx's baggage, or "".
func FromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(Member).Value()
}

// WithSandbox returns ctx with sandbox=<name> set in its baggage.
func WithSandbox(ctx context.Context, name string) (context.Context, error) {
	m, err := baggage.NewMember(Member, name)
	if err != nil {
		return ctx, err
	}
	b, err := baggage.FromContext(ctx).SetMember(m)
	if err != nil {
		return ctx, err
	}
	return baggage.ContextWithBaggage(ctx, b), nil
}

// Middleware extracts trace context and baggage from incoming requests.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := Propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	Propagator.Inject(r.Context(), propagation.HeaderCarrier(r.Header))
	return t.base.RoundTrip(r)
}

// Transport wraps base (default http.DefaultTransport) to inject trace
// context and baggage into outgoing requests.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base}
}

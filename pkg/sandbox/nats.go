package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/propagation"
)

// Bucket is the NATS KV bucket listing which services each sandbox overrides.
// Key: sandbox name. Value: JSON array of service names. kv-sync keeps it in
// step with sandboxes/*.yaml.
const Bucket = "sandboxes"

// Publish sends data on subject with the context's trace context and baggage
// copied into the message headers.
func Publish(ctx context.Context, js jetstream.JetStream, subject string, data []byte) (*jetstream.PubAck, error) {
	msg := nats.NewMsg(subject)
	msg.Data = data
	Propagator.Inject(ctx, propagation.HeaderCarrier(http.Header(msg.Header)))
	return js.PublishMsg(ctx, msg)
}

// ContextFrom restores trace context and baggage from a message's headers.
func ContextFrom(ctx context.Context, h nats.Header) context.Context {
	return Propagator.Extract(ctx, propagation.HeaderCarrier(http.Header(h)))
}

// Overrides tracks which services each sandbox overrides, from the KV bucket.
type Overrides struct {
	mu   sync.RWMutex
	data map[string]map[string]bool
}

// NewOverrides returns an empty set; Watch fills it.
func NewOverrides() *Overrides { return &Overrides{data: map[string]map[string]bool{}} }

// Set replaces one sandbox's services (nil removes the sandbox).
func (o *Overrides) Set(sandbox string, services []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if services == nil {
		delete(o.data, sandbox)
		return
	}
	m := map[string]bool{}
	for _, s := range services {
		m[s] = true
	}
	o.data[sandbox] = m
}

// Overrides reports whether sandbox runs its own copy of svc.
func (o *Overrides) Overrides(sandbox, svc string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.data[sandbox][svc]
}

// Watch keeps o in step with the KV bucket until ctx ends. It returns once
// the initial values are loaded.
func (o *Overrides) Watch(ctx context.Context, js jetstream.JetStream) error {
	kv, err := js.KeyValue(ctx, Bucket)
	if err != nil {
		return fmt.Errorf("sandbox KV bucket %q: %w", Bucket, err)
	}
	w, err := kv.WatchAll(ctx)
	if err != nil {
		return err
	}
	ready := make(chan struct{})
	go func() {
		defer w.Stop()
		once := sync.Once{}
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.Updates():
				if !ok {
					return
				}
				if e == nil { // end of initial values
					once.Do(func() { close(ready) })
					continue
				}
				if e.Operation() != jetstream.KeyValuePut {
					o.Set(e.Key(), nil)
					continue
				}
				var svcs []string
				if err := json.Unmarshal(e.Value(), &svcs); err != nil {
					slog.Warn("bad sandbox KV entry", "key", e.Key(), "err", err)
					continue
				}
				o.Set(e.Key(), svcs)
			}
		}
	}()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("timed out loading sandbox KV bucket")
	}
}

// ShouldProcess decides whether the copy of svc running in own ("" for the
// baseline) handles a message tagged with msgSandbox. A sandbox copy handles
// only its own sandbox's messages. The baseline handles untagged messages and
// those of sandboxes that do not override svc.
func ShouldProcess(o *Overrides, svc, own, msgSandbox string) bool {
	if own != "" {
		return msgSandbox == own
	}
	return msgSandbox == "" || !o.Overrides(msgSandbox, svc)
}

// Consumer consumes a stream as one copy of a service.
type Consumer struct {
	Service string
	// Sandbox is the sandbox this copy runs in, "" for the baseline.
	Sandbox string
	Stream  string
	// Subjects to filter on; empty means all of the stream.
	Subjects []string
	// InactiveThreshold for sandbox consumers, so JetStream removes them
	// after teardown. Default 1h.
	InactiveThreshold time.Duration
	Overrides         *Overrides
}

// Durable returns the consumer name: <svc>, or <svc>-sbx-<name>.
func (c Consumer) Durable() string {
	if c.Sandbox != "" {
		return c.Service + "-sbx-" + c.Sandbox
	}
	return c.Service
}

// Handler processes one message; ctx carries its trace context and baggage.
type Handler func(ctx context.Context, msg jetstream.Msg) error

// Run consumes until ctx ends. Messages for another copy are acked and
// skipped; handler errors nak the message for redelivery.
func (c Consumer) Run(ctx context.Context, js jetstream.JetStream, h Handler) error {
	cfg := jetstream.ConsumerConfig{
		Durable:        c.Durable(),
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverNewPolicy,
		FilterSubjects: c.Subjects,
	}
	if c.Sandbox != "" {
		cfg.InactiveThreshold = c.InactiveThreshold
		if cfg.InactiveThreshold == 0 {
			cfg.InactiveThreshold = time.Hour
		}
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, c.Stream, cfg)
	if err != nil {
		return fmt.Errorf("consumer %s on %s: %w", c.Durable(), c.Stream, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		mctx := ContextFrom(ctx, m.Headers())
		if !ShouldProcess(c.Overrides, c.Service, c.Sandbox, FromContext(mctx)) {
			_ = m.Ack()
			return
		}
		if err := h(mctx, m); err != nil {
			slog.Warn("handler failed", "subject", m.Subject(), "err", err)
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

// EnsureStream creates or updates a limits-retention stream, which lets each
// copy of a service keep its own consumer on the same subjects.
func EnsureStream(ctx context.Context, js jetstream.JetStream, name string, subjects []string) (jetstream.Stream, error) {
	return js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
		Storage:   jetstream.FileStorage,
	})
}

// SyncBucket makes the KV bucket match index (sandbox -> services): puts
// changed entries and deletes sandboxes that are gone.
func SyncBucket(ctx context.Context, js jetstream.JetStream, index map[string][]string) (changed int, err error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: Bucket, History: 1})
	if err != nil {
		return 0, err
	}
	keys, err := kv.ListKeys(ctx)
	if err != nil {
		return 0, err
	}
	existing := map[string]bool{}
	for k := range keys.Keys() {
		existing[k] = true
	}
	names := make([]string, 0, len(index))
	for n := range index {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		svcs := append([]string(nil), index[n]...)
		sort.Strings(svcs)
		want, _ := json.Marshal(svcs)
		if cur, err := kv.Get(ctx, n); err == nil && string(cur.Value()) == string(want) {
			delete(existing, n)
			continue
		}
		if _, err := kv.Put(ctx, n, want); err != nil {
			return changed, err
		}
		changed++
		delete(existing, n)
	}
	for k := range existing {
		if err := kv.Delete(ctx, k); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

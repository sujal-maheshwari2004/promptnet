// Package pubsub is the Phase 4 distribution layer: an embedded NATS server the
// prompt server runs in-process (so it stays a single binary) plus a client to
// publish version-change events. Agents connect to the NATS port and subscribe
// to a prompt's subject to get push notifications; TTL polling (the L1 cache) is
// the pull side. Eventual consistency by design — TTL is the convergence bound.
package pubsub

import (
	"errors"
	"strings"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// Subject maps a prompt URI to its NATS subject:
//
//	promptnet://acme/support/agent  ->  promptnet.acme.support.agent
//
// ponytail: assumes URI path segments are NATS-token-safe (no spaces/dots);
// sanitize here if prompt names ever get exotic.
func Subject(uri string) string {
	path := strings.TrimPrefix(uri, "promptnet://")
	return "promptnet." + strings.ReplaceAll(path, "/", ".")
}

// Bus is an embedded NATS server with a publishing client attached.
type Bus struct {
	ns *natsd.Server
	nc *nats.Conn
}

// NewEmbedded starts an in-process NATS server on host:port and connects a
// publisher to it.
func NewEmbedded(host string, port int) (*Bus, error) {
	ns, err := natsd.NewServer(&natsd.Options{Host: host, Port: port, NoLog: true, NoSigs: true})
	if err != nil {
		return nil, err
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		return nil, errors.New("embedded NATS not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		return nil, err
	}
	return &Bus{ns: ns, nc: nc}, nil
}

// Publish notifies subscribers that the prompt at uri now has versionHash.
func (b *Bus) Publish(uri, versionHash string) error {
	return b.nc.Publish(Subject(uri), []byte(versionHash))
}

// ClientURL is the nats:// address agents connect to.
func (b *Bus) ClientURL() string { return b.ns.ClientURL() }

func (b *Bus) Close() {
	b.nc.Drain()
	b.ns.Shutdown()
}

// Subscriber is the pull-side counterpart to Bus: an agent connects to the NATS
// port the server exposes and gets a callback whenever a prompt it uses changes.
// It hides the subject mapping so embedders never hand-roll NATS subjects.
type Subscriber struct{ nc *nats.Conn }

// Connect dials a NATS server, e.g. nats://127.0.0.1:4222.
func Connect(url string) (*Subscriber, error) {
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, err
	}
	return &Subscriber{nc: nc}, nil
}

// Subscribe calls fn(versionHash) on every version change of the prompt at uri.
func (s *Subscriber) Subscribe(uri string, fn func(versionHash string)) (*nats.Subscription, error) {
	return s.nc.Subscribe(Subject(uri), func(m *nats.Msg) { fn(string(m.Data)) })
}

func (s *Subscriber) Close() { s.nc.Drain() }

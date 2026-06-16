package pubsub

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestSubject(t *testing.T) {
	if got := Subject("promptnet://acme/support/agent"); got != "promptnet.acme.support.agent" {
		t.Errorf("Subject = %q", got)
	}
}

func TestPublishReceive(t *testing.T) {
	bus, err := NewEmbedded("127.0.0.1", -1) // -1 => any free port
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	nc, err := nats.Connect(bus.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	got := make(chan string, 1)
	if _, err := nc.Subscribe(Subject("promptnet://o/r/p"), func(m *nats.Msg) {
		got <- string(m.Data)
	}); err != nil {
		t.Fatal(err)
	}
	nc.Flush() // ensure the subscription is registered before publishing

	if err := bus.Publish("promptnet://o/r/p", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "deadbeef" {
			t.Errorf("got version %q, want deadbeef", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification received")
	}
}

func TestSubscriberHelper(t *testing.T) {
	bus, err := NewEmbedded("127.0.0.1", -1)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	sub, err := Connect(bus.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	got := make(chan string, 1)
	if _, err := sub.Subscribe("promptnet://o/r/p", func(h string) { got <- h }); err != nil {
		t.Fatal(err)
	}
	sub.nc.Flush()

	if err := bus.Publish("promptnet://o/r/p", "cafe123"); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "cafe123" {
			t.Errorf("got %q, want cafe123", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber got no notification")
	}
}

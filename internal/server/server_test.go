package server

import (
	"context"
	"path/filepath"
	"testing"

	pb "promptnet/gen/promptnet/v1"
	"promptnet/internal/store"
)

// fakeNotifier records published events so the test can assert notify-on-change.
type fakeNotifier struct{ events []string }

func (f *fakeNotifier) Publish(uri, hash string) error {
	f.events = append(f.events, uri+" "+hash)
	return nil
}

func newTestServer(t *testing.T) (*Server, *fakeNotifier) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := &fakeNotifier{}
	return NewServer(st, 0, nil, n), n
}

func TestPublishThenGet(t *testing.T) {
	s, n := newTestServer(t)
	ctx := context.Background()
	const uri = "promptnet://o/r/p"

	pubResp, err := s.PublishPrompt(ctx, &pb.PublishPromptRequest{
		Uri: uri, Template: "Hi {name}", Slots: []string{"name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pubResp.GetVersionHash() == "" {
		t.Fatal("expected a version hash")
	}
	if len(n.events) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(n.events))
	}

	got, err := s.GetPrompt(ctx, &pb.GetPromptRequest{Uri: uri})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetTemplate() != "Hi {name}" || got.GetVersionHash() != pubResp.GetVersionHash() {
		t.Fatalf("GetPrompt mismatch: %+v", got)
	}

	// Republishing identical content is a no-op: no extra notification.
	if _, err := s.PublishPrompt(ctx, &pb.PublishPromptRequest{
		Uri: uri, Template: "Hi {name}", Slots: []string{"name"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(n.events) != 1 {
		t.Fatalf("identical republish should not notify; got %d events", len(n.events))
	}
}

func TestPublishInvalidRejected(t *testing.T) {
	s, _ := newTestServer(t)
	// Template uses {org} but it isn't declared -> validation must reject.
	_, err := s.PublishPrompt(context.Background(), &pb.PublishPromptRequest{
		Uri: "promptnet://o/r/p", Template: "Hi {org}", Slots: []string{"name"},
	})
	if err == nil {
		t.Fatal("expected invalid prompt to be rejected")
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.GetPrompt(context.Background(), &pb.GetPromptRequest{Uri: "promptnet://x/y/z"}); err == nil {
		t.Fatal("expected NotFound for missing prompt")
	}
}

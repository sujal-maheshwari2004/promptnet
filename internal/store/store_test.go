package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestPutGetRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.Get(ctx, "promptnet://missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	in := Prompt{URI: "promptnet://o/r/p", Template: "Hi {name}", Slots: []string{"name"}}
	if err := st.Put(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, in.URI)
	if err != nil {
		t.Fatal(err)
	}
	if got.Template != in.Template || len(got.Slots) != 1 || got.Slots[0] != "name" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.VersionHash != Hash(in.Template, in.Slots) {
		t.Error("version hash not persisted correctly")
	}

	// Upsert: same URI, new content overwrites.
	if err := st.Put(ctx, Prompt{URI: in.URI, Template: "Bye {name}", Slots: []string{"name"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get(ctx, in.URI)
	if got.Template != "Bye {name}" {
		t.Fatalf("upsert did not overwrite: %q", got.Template)
	}
}

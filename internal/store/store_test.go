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

func TestMigrateAndDump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion() != len(migrations) {
		t.Fatalf("want version %d, got %d", len(migrations), st.SchemaVersion())
	}
	ctx := context.Background()
	if err := st.Put(ctx, Prompt{URI: "promptnet://o/r/p", Template: "Hi {n}", Slots: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	dump, err := st.Dump(ctx)
	if err != nil || len(dump) != 1 || dump[0].URI != "promptnet://o/r/p" {
		t.Fatalf("dump = %+v, err %v", dump, err)
	}
	st.Close()

	// Reopening is idempotent: migrations don't re-run, version is unchanged.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if st2.SchemaVersion() != len(migrations) {
		t.Fatalf("reopen bumped version to %d", st2.SchemaVersion())
	}
}

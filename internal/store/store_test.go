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

func TestCommitGraph(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const uri = "promptnet://o/r/p"

	// Two commits on main form a chain: c2.Parent == c1.
	c1, err := st.Commit(ctx, uri, DefaultBranch, "v1", nil, "ann", "init")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := st.Commit(ctx, uri, DefaultBranch, "v2", nil, "ann", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetCommit(ctx, c2); got.Parent != c1 {
		t.Fatalf("c2 parent = %q, want %q", got.Parent, c1)
	}

	// Branch off main, commit on the branch — main is untouched.
	if err := st.Branch(ctx, uri, "feature", DefaultBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Commit(ctx, uri, "feature", "v3-feat", nil, "bob", "feat"); err != nil {
		t.Fatal(err)
	}
	mainLog, _ := st.Log(ctx, uri, DefaultBranch)
	if len(mainLog) != 2 {
		t.Fatalf("main log len = %d, want 2", len(mainLog))
	}

	// Merge feature into main: merge commit has two parents and takes feature's content.
	mh, err := st.Merge(ctx, uri, DefaultBranch, "feature", "ann", "")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := st.GetCommit(ctx, mh)
	if m.Parent != c2 || m.Parent2 == "" {
		t.Fatalf("merge parents = (%q,%q), want first=%q and a second", m.Parent, m.Parent2, c2)
	}
	if m.Template != "v3-feat" {
		t.Fatalf("merge content = %q, want feature's v3-feat", m.Template)
	}
	if head, _ := st.Log(ctx, uri, DefaultBranch); head[0].Hash != mh {
		t.Fatal("main tip is not the merge commit")
	}

	if err := st.Branch(ctx, uri, "x", "nope"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("branch from missing = %v, want ErrBranchNotFound", err)
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

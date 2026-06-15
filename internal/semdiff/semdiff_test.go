package semdiff

import (
	"strings"
	"testing"
)

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

func TestAnalyzeMechanics(t *testing.T) {
	emb := LexicalEmbedder{}

	// Identical versions -> no changes at all.
	base := lines("one alpha\ntwo beta\nthree gamma\nfour delta\nfive epsilon\nsix zeta\nseven eta")
	if res, err := Analyze(emb, base, base); err != nil || len(res) != 0 {
		t.Fatalf("identical: want 0 changes, got %d (err %v)", len(res), err)
	}

	// One isolated middle-line change in clean context.
	edited := append([]string(nil), base...)
	edited[3] = "four totally unrelated replacement words"
	res, err := Analyze(emb, base, edited)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 change, got %d", len(res))
	}
	r := res[0]
	if r.Signal2 <= 0 {
		t.Fatalf("Signal2 should be > 0, got %.3f", r.Signal2)
	}
	// Dilution: as the window grows over unchanged context, the delta must not
	// climb — an isolated change ripples less, not more.
	prev := r.Signal2
	for _, w := range r.Up.Curve {
		if w.Delta > prev+flattenEps {
			t.Fatalf("up curve climbed on isolated change: %.3f > %.3f", w.Delta, prev)
		}
		prev = w.Delta
	}
	if r.Class == "structural" {
		t.Fatalf("isolated change should not read as structural, got %q", r.Class)
	}
}

func TestClassify(t *testing.T) {
	flat := Direction{Curve: []Window{{2, 0.10}}, StoppedAtBoundary: false}
	decayed := Direction{Curve: []Window{{2, 0.30}, {4, 0.08}}, StoppedAtBoundary: true}
	persistent := Direction{Curve: []Window{{2, 0.45}, {4, 0.44}}, StoppedAtBoundary: true}

	if got := classify(0.5, persistent, flat); got != "structural" {
		t.Errorf("persistent ripple should be structural, got %q", got)
	}
	if got := classify(0.5, decayed, flat); got != "localized tweak" {
		t.Errorf("decayed ripple with high point delta should be localized, got %q", got)
	}
	if got := classify(0.05, flat, flat); got != "minor edit" {
		t.Errorf("tiny point delta should be minor, got %q", got)
	}
}

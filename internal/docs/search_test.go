package docs

import (
	"reflect"
	"strings"
	"testing"
)

func TestTerms(t *testing.T) {
	t.Parallel()
	for q, want := range map[string][]string{
		"  Restart   the POD ": {"restart", "the", "pod"},
		"pod pod Pod":          {"pod"},
		"":                     nil,
		"a b c d e f g h i j":  {"a", "b", "c", "d", "e", "f", "g", "h"},
	} {
		if got := Terms(q); !reflect.DeepEqual(got, want) {
			t.Errorf("Terms(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestFind(t *testing.T) {
	t.Parallel()
	md := "# Runbook\n\nWhen the queue backs up:\n\n- Restart the worker pod.\n- Then check the Queue depth.\n"

	m, ok := Find(md, "Runbook", Terms("queue restart"), 3)
	if !ok {
		t.Fatal("no match")
	}
	want := []Line{{3, "When the queue backs up:"}, {5, "- Restart the worker pod."}, {6, "- Then check the Queue depth."}}
	if !reflect.DeepEqual(m.Lines, want) || m.Score != 3 {
		t.Errorf("match = %+v", m)
	}

	// Every word must be on the page, though not on one line.
	if _, ok := Find(md, "Runbook", Terms("queue database"), 3); ok {
		t.Error("matched a page without 'database'")
	}
	// A word in the title ranks the page above one that only mentions it.
	title, _ := Find(md, "Runbook", Terms("runbook"), 3)
	body, _ := Find("# Ops\nsee the runbook\n", "Ops", Terms("runbook"), 3)
	if title.Score <= body.Score {
		t.Errorf("title %d <= body %d", title.Score, body.Score)
	}
	// At most max lines are returned, the score still counts them all.
	many, _ := Find(strings.Repeat("pod\n", 10), "x", Terms("pod"), 3)
	if len(many.Lines) != 3 || many.Score != 10 {
		t.Errorf("many = %+v", many)
	}
}

func TestFindClipsLongLines(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("é", 300) + " needle " + strings.Repeat("ü", 300)
	m, ok := Find(line, "x", Terms("NEEDLE"), 3)
	if !ok || len(m.Lines) != 1 {
		t.Fatalf("match = %+v", m)
	}
	got := m.Lines[0].Text
	if !strings.Contains(got, "needle") || len([]rune(got)) > maxLineRunes+2 || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("clipped = %q (%d runes)", got, len([]rune(got)))
	}
}

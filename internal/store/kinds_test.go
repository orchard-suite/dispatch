package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestKindsAreDeclaredReplacedAndChosen(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	approval := notice.DeclaredKind{Kind: "action.awaiting_approval", Title: "A request waits for your approval", Description: "Someone asked for a change you can approve.", Weight: notice.WeightNotify}
	finished := notice.DeclaredKind{Kind: "action.finished", Title: "A run ended", Weight: notice.WeightNotice}
	if dropped, err := st.DeclareKinds(ctx, "approvals", []notice.DeclaredKind{approval, finished}, at); err != nil || dropped != 0 {
		t.Fatalf("declare: %d %v", dropped, err)
	}
	if _, err := st.DeclareKinds(ctx, "alpha", []notice.DeclaredKind{{Kind: "alpha.changed", Title: "A thing changed", Weight: notice.WeightPage}}, at); err != nil {
		t.Fatal(err)
	}
	kinds, err := st.DeclaredKinds(ctx)
	if err != nil || len(kinds) != 3 {
		t.Fatalf("kinds %+v %v", kinds, err)
	}
	// By producer, then kind, each with the producer that declared it.
	approval.Producer = "approvals"
	if kinds[0].Producer != "alpha" || kinds[1] != approval || kinds[2].Kind != "action.finished" {
		t.Errorf("listed %+v", kinds)
	}

	// A kind another producer holds is refused by name, and nothing changes.
	_, err = st.DeclareKinds(ctx, "alpha", []notice.DeclaredKind{{Kind: "alpha.new", Title: "New", Weight: notice.WeightNotice}, {Kind: "action.finished", Title: "Mine now", Weight: notice.WeightPage}}, at)
	if !errors.Is(err, ErrKindTaken) || err.Error() != "another producer declared this kind: action.finished" {
		t.Fatalf("taken: %v", err)
	}
	if kinds, _ := st.DeclaredKinds(ctx); len(kinds) != 3 || kinds[0].Kind != "alpha.changed" {
		t.Errorf("a refused set changed something: %+v", kinds)
	}

	// A choice needs a person in the directory and a declared kind.
	if err := st.SetChoice(ctx, "a@example.com", notice.Choice{Kind: "action.finished", Media: []notice.Medium{notice.Email}}, at); err == nil {
		t.Error("a choice was kept for nobody")
	}
	if err := st.LearnPerson(ctx, "a@example.com", at); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChoice(ctx, "a@example.com", notice.Choice{Kind: "action.unknown"}, at); err == nil {
		t.Error("a choice was kept for a kind nobody declared")
	}
	for _, c := range []notice.Choice{
		{Kind: "action.finished", Media: []notice.Medium{notice.Email, notice.SMS}},
		{Kind: "action.awaiting_approval"},
	} {
		if err := st.SetChoice(ctx, "a@example.com", c, at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Choices(ctx, "a@example.com")
	if err != nil || len(got) != 2 || len(got["action.finished"].Media) != 2 || !got["action.awaiting_approval"].ListOnly() {
		t.Fatalf("choices %+v %v", got, err)
	}
	// Only the producer that declared the kind is answered.
	if c, err := st.Choice(ctx, "a@example.com", "approvals", "action.finished"); err != nil || c.Media[1] != notice.SMS {
		t.Errorf("choice %+v %v", c, err)
	}
	if _, err := st.Choice(ctx, "a@example.com", "alpha", "action.finished"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another producer's event: %v", err)
	}
	if _, err := st.Choice(ctx, "b@example.com", "approvals", "action.finished"); !errors.Is(err, ErrNotFound) {
		t.Errorf("someone who set nothing: %v", err)
	}

	// Declaring again replaces the set: a kept kind keeps its choices and
	// takes its new words, a dropped kind takes its choices with it.
	finished.Title = "A run ended, well or not"
	if dropped, err := st.DeclareKinds(ctx, "approvals", []notice.DeclaredKind{finished}, at); err != nil || dropped != 1 {
		t.Fatalf("replace: %d %v", dropped, err)
	}
	kinds, _ = st.DeclaredKinds(ctx)
	if len(kinds) != 2 || kinds[1].Title != "A run ended, well or not" {
		t.Errorf("after replacing: %+v", kinds)
	}
	if got, _ := st.Choices(ctx, "a@example.com"); len(got) != 1 || len(got["action.finished"].Media) != 2 {
		t.Errorf("choices after replacing: %+v", got)
	}
	if err := st.ClearChoice(ctx, "a@example.com", "action.finished"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Choices(ctx, "a@example.com"); len(got) != 0 {
		t.Errorf("cleared: %+v", got)
	}
	// An empty set drops everything the producer declared.
	if dropped, err := st.DeclareKinds(ctx, "approvals", nil, at); err != nil || dropped != 1 {
		t.Fatalf("empty: %d %v", dropped, err)
	}
	// Removing a person takes their choices.
	if err := st.SetChoice(ctx, "a@example.com", notice.Choice{Kind: "alpha.changed"}, at); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePerson(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Choices(ctx, "a@example.com"); len(got) != 0 {
		t.Errorf("a removed person kept choices: %+v", got)
	}
}

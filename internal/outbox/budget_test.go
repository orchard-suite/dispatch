package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestAPersonOverBudgetGetsNotifyInTheMorning(t *testing.T) {
	o, m, _, now, _ := peopleSetup(t)
	ctx := context.Background()
	settings, _ := o.store.Settings(ctx)
	settings.PersonPages, settings.Zone = 1, "UTC"
	o.store.SaveSettings(ctx, settings)
	if _, err := o.Accept(ctx, event("p", notice.Target{Chain: "oncall"})); err != nil {
		t.Fatal(err)
	}
	o.SendDue(ctx)
	if got, _ := o.store.PagesOf(ctx, "ann@example.com", now.Add(-time.Hour)); got != 1 {
		t.Fatalf("pages of Ann: %d", got)
	}
	o.Acknowledge(ctx, 1, "ann@example.com")
	sent := len(m.to())
	if _, err := o.Accept(ctx, notice.Event{Producer: "ci", Kind: "build.failed", Key: "b", Title: "Build failed", To: []notice.Target{{Person: "ann@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	o.SendDue(ctx)
	if len(m.to()) != sent {
		t.Fatal("notify went at once though Ann is over budget")
	}
	*now = time.Date(now.Year(), now.Month(), now.Day()+1, 7, 1, 0, 0, time.UTC)
	o.SendDue(ctx)
	if len(m.to()) != sent+1 {
		t.Errorf("notify goes in the morning: %v", m.to())
	}
}

package desk

import (
	"testing"

	"github.com/orchard-suite/dispatch/internal/incident"
)

func TestUpdateTemplateKeepsOneStop(t *testing.T) {
	d := newDesk(t)
	got := d.UpdateTemplate(t.Context(), incident.Incident{Title: "The queue is stuck.", Service: "shop", State: incident.Open, Severity: incident.Warning})
	want := "We are looking into The queue is stuck. shop is open. Next update within the hour."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

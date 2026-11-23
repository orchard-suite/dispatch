package notice

import (
	"strings"
)

// Text is an event as one short text: its title, the weight's word, and
// for a page, how to answer and the link that acknowledges it.
func Text(e Event, ack string) string {
	var b strings.Builder
	switch {
	case ack != "":
		b.WriteString("PAGE: ")
	case e.Severity == "critical":
		b.WriteString("CRITICAL: ")
	}
	b.WriteString(strings.TrimSpace(e.Title))
	if ack != "" {
		b.WriteString("\nReply ACK to take it, ESC to pass it on. ")
		b.WriteString(ack)
	} else if len(e.Links) > 0 {
		b.WriteString("\n")
		b.WriteString(e.Links[0].URL)
	}
	return b.String()
}

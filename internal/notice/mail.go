package notice

import (
	"bytes"
	"html/template"
	"strings"
)

// Letter is an event as an email: a subject, an HTML body, and the same in
// plain text.
type Letter struct {
	Subject string
	HTML    string
	Text    string
}

var letterHTML = template.Must(template.New("letter").Parse(`<!doctype html>
<html><body style="font-family:Segoe UI,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1a1a19">
<h2 style="margin:0 0 8px;font-size:18px">{{.E.Title}}</h2>
{{if .E.Text}}<p style="margin:0 0 12px;white-space:pre-wrap">{{.E.Text}}</p>{{end}}
{{if .Facts}}<table style="border-collapse:collapse;margin:0 0 12px">{{range .Facts}}<tr><td style="padding:2px 12px 2px 0;color:#6b6a63">{{.Name}}</td><td style="padding:2px 0">{{.Value}}</td></tr>{{end}}</table>{{end}}
{{if .Ack}}<p style="margin:0 0 12px"><a href="{{.Ack}}" style="display:inline-block;padding:8px 14px;background:#1a1a19;color:#fff;text-decoration:none;border-radius:6px">Acknowledge</a></p>{{end}}
{{if .E.Links}}<p style="margin:0 0 12px">{{range $i, $l := .E.Links}}{{if $i}} · {{end}}<a href="{{$l.URL}}">{{$l.Title}}</a>{{end}}</p>{{end}}
<p style="margin:16px 0 0;font-size:12px;color:#6b6a63">From {{.E.Producer}} through dispatch · {{.When}}</p>
</body></html>`))

// Mail is the letter a person is sent for the event. ack, when set, is the
// link that acknowledges the chain's page the letter was sent for. An event
// that brings its own page is sent as that page; the plain text stays the
// event's own words.
func Mail(e Event, ack string) Letter {
	facts := letterFacts(e)
	when := e.At.UTC().Format("2006-01-02 15:04 UTC")
	var buf bytes.Buffer
	_ = letterHTML.Execute(&buf, map[string]any{"E": e, "Facts": facts, "Ack": ack, "When": when})

	var t strings.Builder
	t.WriteString(e.Title + "\n\n")
	if e.Text != "" {
		t.WriteString(e.Text + "\n\n")
	}
	for _, f := range facts {
		t.WriteString(f.Name + ": " + f.Value + "\n")
	}
	if len(facts) > 0 {
		t.WriteString("\n")
	}
	if ack != "" {
		t.WriteString("Acknowledge: " + ack + "\n")
	}
	for _, l := range e.Links {
		t.WriteString(l.Title + ": " + l.URL + "\n")
	}
	t.WriteString("\nFrom " + e.Producer + " through dispatch, " + when + "\n")

	subject := e.Title
	switch e.Severity {
	case "critical":
		subject = "[critical] " + subject
	case "warning":
		subject = "[warning] " + subject
	}
	html := buf.String()
	if e.Page != "" {
		html = e.Page
	}
	return Letter{Subject: subject, HTML: html, Text: t.String()}
}

func letterFacts(e Event) []Fact {
	facts := make([]Fact, 0, len(e.Facts)+2)
	if e.Entity != nil {
		name := e.Entity.Title
		if name == "" {
			name = e.Entity.Key
		}
		facts = append(facts, Fact{Name: "About", Value: name})
	}
	facts = append(facts, e.Facts...)
	if e.Severity != "" {
		facts = append(facts, Fact{Name: "Severity", Value: e.Severity})
	}
	return facts
}

// WithAck is the event with a link that acknowledges its chain's page,
// first among its links, for a card.
func WithAck(e Event, ack string) Event {
	if ack == "" {
		return e
	}
	e.Links = append([]Link{{Title: "Acknowledge", URL: ack}}, e.Links...)
	return e
}

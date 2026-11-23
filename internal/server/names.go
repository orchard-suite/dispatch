package server

import (
	"context"
	"html/template"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/orchard-suite/dispatch/internal/store"
)

// directory is the desk's people by address, so a page names a person and
// never shows their address. Every page render refreshes it.
var directory atomic.Pointer[map[string]string]

// learnNames refreshes the names pages use. A failed read keeps the last.
func learnNames(ctx context.Context, st *store.Store) {
	people, err := st.People(ctx)
	if err != nil {
		return
	}
	m := make(map[string]string, len(people))
	for _, p := range people {
		if p.Name != "" {
			m[strings.ToLower(p.Email)] = p.Name
		}
	}
	directory.Store(&m)
}

// personName is a person's name: the directory's, or one made from the address's
// local part. An actor that is not an address (a token, a machine) is
// itself.
func personName(s string) string {
	if s == "" {
		return "nobody"
	}
	at := strings.Index(s, "@")
	if at < 0 {
		return s
	}
	if m := directory.Load(); m != nil {
		if n := (*m)[strings.ToLower(s)]; n != "" {
			return n
		}
	}
	words := strings.FieldsFunc(s[:at], func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '+' })
	for i, w := range words {
		w = strings.TrimRightFunc(w, unicode.IsDigit)
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	if n := strings.TrimSpace(strings.Join(words, " ")); n != "" {
		return n
	}
	return s[:at]
}

// initials are up to two letters for a person's avatar.
func initials(s string) string {
	if s == "" {
		return "?"
	}
	var out []rune
	for _, w := range strings.Fields(personName(s)) {
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				out = append(out, unicode.ToUpper(r))
				break
			}
		}
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

var addressInText = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// named replaces every address in a line of text with its person's name.
func named(s string) string {
	return addressInText.ReplaceAllStringFunc(s, personName)
}

var wholeToken = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// whole escapes a line of text and keeps each date in it on one line, so a
// title never wraps at the hyphens of "2026-11-08".
func whole(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range wholeToken.FindAllStringIndex(s, -1) {
		b.WriteString(template.HTMLEscapeString(s[last:m[0]]))
		b.WriteString(`<span class="nw">` + template.HTMLEscapeString(s[m[0]:m[1]]) + `</span>`)
		last = m[1]
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return template.HTML(b.String())
}

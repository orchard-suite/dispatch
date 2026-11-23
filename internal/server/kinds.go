package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// maxKindsBody is the largest body PUT /v1/kinds reads: a full set of
// kinds, each with the longest title and description, fits.
const maxKindsBody = 512 << 10

// apiKinds takes the whole set of event kinds a producer sends, in words
// people read on Me when they choose how to be told of each. The producer
// is the token's name, as for an event. A personal token may not: anyone
// on the desk can make one, and a declared kind shows on everyone's Me.
func (s *Server) apiKinds(w http.ResponseWriter, r *http.Request, by caller) {
	ctx := r.Context()
	if p, ok := ctx.Value(personKey{}).(string); ok && p != "" {
		s.logRequest(ctx, slog.LevelWarn, "kinds refused: a personal token", r)
		writeError(w, http.StatusForbidden, "a personal token cannot declare kinds; use a token from Settings")
		return
	}
	var body struct {
		Kinds *[]notice.DeclaredKind `json:"kinds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKindsBody)).Decode(&body); err != nil || body.Kinds == nil {
		writeError(w, http.StatusBadRequest, `the body must be {"kinds": [...]} as JSON`)
		return
	}
	producer, kinds := string(by), *body.Kinds
	if err := notice.ValidateKinds(kinds); err != nil {
		s.logRequest(ctx, slog.LevelWarn, "kinds refused", r, "producer", producer, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	dropped, err := s.store.DeclareKinds(ctx, producer, kinds, now)
	if errors.Is(err, store.ErrKindTaken) {
		s.logRequest(ctx, slog.LevelWarn, "kinds refused", r, "producer", producer, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.failed(w, r, err, "kinds") {
		return
	}
	s.logRequest(ctx, slog.LevelInfo, "kinds declared", r, "producer", producer, "kinds", len(kinds), "dropped", dropped)
	s.desk.Annals().Decide(ctx, annals.Decision{Verb: "kinds.declare", Kind: "producer", ID: producer, By: incident.System(producer),
		Key:    fmt.Sprintf("producer:%s:kinds.declare:%d", producer, now.UnixNano()),
		Labels: map[string]string{"kinds": fmt.Sprint(len(kinds)), "dropped": fmt.Sprint(dropped)}, At: now})
	writeJSON(w, http.StatusOK, map[string]int{"kinds": len(kinds)})
}

// kindGrid is the grid on Me: one row per declared kind, grouped by the
// product that sends it, and one column per medium the desk can send.
type kindGrid struct {
	Media  []gridMedium
	Groups []kindGroup
	// Cols is the tick columns: every medium, and List only.
	Cols int
}

// gridMedium is one column: a medium the desk sends, and whether this
// person has it set up. Why says what is missing when they do not.
type gridMedium struct {
	M    notice.Medium
	Name string
	Has  bool
	Why  string
}

type kindGroup struct {
	Producer string
	Rows     []kindRow
}

// kindRow is one kind as the person set it. Fixed is a kind sent as a
// page, which no choice turns down. Rule is what their rules give the
// kind's weight, which an untouched row follows.
type kindRow struct {
	notice.DeclaredKind
	Fixed bool
	List  bool
	On    map[notice.Medium]bool
	Rule  string
}

// grid builds the Me grid for one person.
func (s *Server) grid(r *http.Request, p notice.Person) (kindGrid, error) {
	ctx := r.Context()
	kinds, err := s.store.DeclaredKinds(ctx)
	if err != nil {
		return kindGrid{}, err
	}
	choices, err := s.store.Choices(ctx, p.Email)
	if err != nil {
		return kindGrid{}, err
	}
	g := kindGrid{}
	can := s.sendable()
	for _, m := range notice.Media {
		if !can[m] {
			continue
		}
		col := gridMedium{M: m, Name: mediumName(m), Has: p.Has(m)}
		switch {
		case col.Has:
		case m == notice.SMS:
			col.Why = "Add an SMS number below to use this"
		case m == notice.Discord:
			col.Why = "Add a Discord account below to use this"
		default:
			col.Why = "The desk has no address for you"
		}
		g.Media = append(g.Media, col)
	}
	g.Cols = len(g.Media) + 1
	for _, k := range kinds {
		row := kindRow{DeclaredKind: k, Fixed: k.Weight == notice.WeightPage, On: map[notice.Medium]bool{}}
		var sends []notice.RuleStep
		for _, st := range p.Rules.For(k.Weight, p) {
			if can[st.Medium] && p.Has(st.Medium) {
				sends = append(sends, st)
			}
		}
		row.Rule = "list only"
		if len(sends) > 0 {
			row.Rule = ruleSay(sends)
		}
		if c, ok := choices[k.Kind]; ok && !row.Fixed {
			row.List = c.ListOnly()
			for _, m := range c.Media {
				row.On[m] = true
			}
		}
		if n := len(g.Groups); n == 0 || g.Groups[n-1].Producer != k.Producer {
			g.Groups = append(g.Groups, kindGroup{Producer: k.Producer})
		}
		last := &g.Groups[len(g.Groups)-1]
		last.Rows = append(last.Rows, row)
	}
	return g, nil
}

// formKinds saves how the signed-in person asked to be told of each kind
// of event. A row with media ticked is told by those; List only is told
// nowhere; a row with nothing ticked goes back to their rules by weight.
// A kind sent as a page is never taken: a page goes as their page rule
// says. Only rows that changed are saved, and each is a decision.
func (s *Server) formKinds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.renderMe(w, r, http.StatusBadRequest, "the form could not be read", "")
		return
	}
	email, err := notice.NormalEmail(door.From(ctx).Email)
	if err != nil {
		s.renderMe(w, r, http.StatusBadRequest, "the front door gave no address", "")
		return
	}
	p, err := s.store.Person(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		p, err = notice.Person{Email: email}, nil
	}
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	declared, err := s.store.DeclaredKinds(ctx)
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	old, err := s.store.Choices(ctx, email)
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	byKind := map[string]notice.DeclaredKind{}
	for _, k := range declared {
		byKind[k.Kind] = k
	}
	can := s.sendable()
	// change is one row that differs from what is kept: a new choice, or
	// clear to go back to the person's rules.
	type change struct {
		choice notice.Choice
		clear  bool
	}
	var changes []change
	for _, kind := range r.Form["kind"] {
		k, ok := byKind[kind]
		if !ok || k.Weight == notice.WeightPage {
			// A kind its producer dropped since the page was drawn, or one
			// sent as a page: there is nothing to choose.
			continue
		}
		list := r.Form.Get("list_"+kind) != ""
		var media []notice.Medium
		for _, word := range r.Form["m_"+kind] {
			m, ok := notice.ParseMedium(word)
			if !ok || !can[m] || !p.Has(m) {
				s.renderMe(w, r, http.StatusBadRequest, k.Title+": "+word+" is not a way the desk can reach you", "")
				return
			}
			if !slices.Contains(media, m) {
				media = append(media, m)
			}
		}
		if list && len(media) > 0 {
			s.renderMe(w, r, http.StatusBadRequest, k.Title+": choose List only or ways to be told, not both", "")
			return
		}
		had, set := old[kind]
		switch {
		case !list && len(media) == 0:
			if set {
				changes = append(changes, change{choice: notice.Choice{Kind: kind}, clear: true})
			}
		case !set || !sameMedia(had.Media, media):
			changes = append(changes, change{choice: notice.Choice{Kind: kind, Media: media}})
		}
	}
	if len(changes) == 0 {
		http.Redirect(w, r, "/me?saved", http.StatusSeeOther)
		return
	}
	now := time.Now()
	// A choice hangs off the person's row, so the first save makes one for
	// someone the directory has not met. It holds no role and no rule.
	if s.pageFailed(w, r, s.store.LearnPerson(ctx, email, now), "Me") {
		return
	}
	for _, c := range changes {
		said := "rules"
		if c.clear {
			err = s.store.ClearChoice(ctx, email, c.choice.Kind)
		} else {
			err = s.store.SetChoice(ctx, email, c.choice, now)
			said = "list"
			if !c.choice.ListOnly() {
				words := make([]string, 0, len(c.choice.Media))
				for _, m := range c.choice.Media {
					words = append(words, string(m))
				}
				said = strings.Join(words, ",")
			}
		}
		if s.pageFailed(w, r, err, "Me") {
			return
		}
		s.logRequest(ctx, slog.LevelInfo, "person chose how they are told of a kind", r, "kind", c.choice.Kind, "media", said)
		s.desk.Annals().Decide(ctx, annals.Decision{Verb: "person.choose", Kind: "person", ID: email, By: incident.Person(email),
			Key:    fmt.Sprintf("person:%s:person.choose:%s:%d", email, c.choice.Kind, now.UnixNano()),
			Labels: map[string]string{"kind": c.choice.Kind, "media": said}, At: now})
	}
	http.Redirect(w, r, "/me?saved", http.StatusSeeOther)
}

// sameMedia says whether two choices name the same media, in any order.
func sameMedia(a, b []notice.Medium) bool {
	if len(a) != len(b) {
		return false
	}
	for _, m := range a {
		if !slices.Contains(b, m) {
			return false
		}
	}
	return true
}

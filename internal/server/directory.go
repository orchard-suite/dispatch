package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// learn keeps everyone with a desk role as a person the desk can reach,
// once per process. Someone with no desk role is kept only when they
// first save something on Me.
func (s *Server) learn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email := strings.ToLower(door.From(r.Context()).Email)
		if _, known := s.learned.Load(email); !known && email != "" && s.onDesk(r) {
			if _, err := notice.NormalEmail(email); err == nil {
				if err := s.store.LearnPerson(r.Context(), email, time.Now()); err != nil {
					s.logRequest(r.Context(), slog.LevelError, "person not learned", r, "error", err)
				} else {
					s.learned.Store(email, true)
				}
			}
		}
		next(w, r)
	}
}

// chainLine is a chain as the People page lists it: its steps by name,
// and written the way the form takes them.
type chainLine struct {
	notice.Chain
	Say  []stepLine
	Text string
}

// stepLine is one step of a chain as a person reads it: who is told, by
// name, and how long the chain waits for an acknowledgement.
type stepLine struct {
	N    int
	Wait int
	Who  []string
}

// namer names targets: a person by name, a group, chain, or schedule by
// what it is called. A failed read leaves the slugs.
type namer struct{ groups, chains, schedules map[string]string }

func (s *Server) namer(r *http.Request) namer {
	ctx := r.Context()
	n := namer{groups: map[string]string{}, chains: map[string]string{}, schedules: map[string]string{}}
	if gs, err := s.store.Groups(ctx); err == nil {
		for _, g := range gs {
			n.groups[g.Slug] = g.Name
		}
	}
	if cs, err := s.store.Chains(ctx); err == nil {
		for _, c := range cs {
			n.chains[c.Slug] = c.Name
		}
	}
	if ss, err := s.store.Schedules(ctx, time.Now()); err == nil {
		for _, sc := range ss {
			n.schedules[sc.Slug] = sc.Name
		}
	}
	return n
}

func nameOr(m map[string]string, slug string) string {
	if n := m[slug]; n != "" {
		return n
	}
	return slug
}

// target is who a target is, as a sentence names it.
func (n namer) target(t notice.Target) string {
	switch {
	case t.Person != "":
		return personName(t.Person)
	case t.Group != "":
		return "the group " + nameOr(n.groups, t.Group)
	case t.Chain != "":
		return "the chain " + nameOr(n.chains, t.Chain)
	case t.Schedule != "":
		return "whoever is on " + nameOr(n.schedules, t.Schedule)
	}
	return "nobody"
}

func (n namer) steps(steps []notice.Step) []stepLine {
	out := make([]stepLine, 0, len(steps))
	for i, st := range steps {
		l := stepLine{N: i + 1, Wait: st.Wait}
		for _, t := range st.Targets {
			l.Who = append(l.Who, n.target(t))
		}
		out = append(out, l)
	}
	return out
}

// groupLine is a group as the People page lists it: who is in it, and
// for its form, which known people are and the addresses that are not.
type groupLine struct {
	notice.Group
	In     map[string]bool
	Others string
}

func lineGroup(g notice.Group, people []notice.Person) groupLine {
	l := groupLine{Group: g, In: map[string]bool{}}
	known := map[string]bool{}
	for _, p := range people {
		known[p.Email] = true
	}
	var others []string
	for _, m := range g.Members {
		l.In[m] = true
		if !known[m] {
			others = append(others, m)
		}
	}
	l.Others = strings.Join(others, ", ")
	return l
}

func (s *Server) pagePeople(w http.ResponseWriter, r *http.Request) {
	s.renderPeople(w, r, http.StatusOK, "", "")
}

// renderPeople shows the people, groups, and chains. bad is a form's
// refusal; done says what a form did.
func (s *Server) renderPeople(w http.ResponseWriter, r *http.Request, status int, bad, done string) {
	ctx := r.Context()
	people, err := s.store.People(ctx)
	if s.pageFailed(w, r, err, "People") {
		return
	}
	groups, err := s.store.Groups(ctx)
	if s.pageFailed(w, r, err, "People") {
		return
	}
	chains, err := s.store.Chains(ctx)
	if s.pageFailed(w, r, err, "People") {
		return
	}
	sends := s.sendable()
	var by, not []medium
	for _, m := range notice.Media {
		if sends[m] {
			by = append(by, medium{M: m, Name: mediumName(m)})
		} else {
			not = append(not, medium{M: m, Name: mediumName(m)})
		}
	}
	n := s.namer(r)
	lines := make([]chainLine, 0, len(chains))
	for _, c := range chains {
		lines = append(lines, chainLine{Chain: c, Say: n.steps(c.Steps), Text: notice.FormatSteps(c.Steps)})
	}
	glines := make([]groupLine, 0, len(groups))
	for _, g := range groups {
		glines = append(glines, lineGroup(g, people))
	}
	plines := make([]personLine, 0, len(people))
	for _, p := range people {
		plines = append(plines, linePerson(p))
	}
	s.render(w, r, status, "people", map[string]any{
		"People": plines, "Groups": glines, "Chains": lines, "Media": notice.Media, "Bad": bad, "Done": done,
		"Sends": sends, "SendsBy": by, "SendsNot": not,
	})
}

// refused shows a refused People form, and anything else as a failure.
func (s *Server) refused(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, notice.ErrInvalid) {
		s.renderPeople(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": "), "")
		return true
	}
	return s.pageFailed(w, r, err, "People")
}

// personFromForm changes a person's own settings by what a form carries:
// name, SMS number, Discord account, a rule line for each weight (blank
// keeps the default), and quiet hours. A field the form does not carry
// keeps the base's value, so a row that edits one setting changes only it.
func personFromForm(r *http.Request, base notice.Person) (notice.Person, error) {
	p := base
	has := func(k string) bool { _, ok := r.PostForm[k]; return ok }
	if has("name") {
		p.Name = strings.TrimSpace(r.PostFormValue("name"))
	}
	if has("discord_id") {
		p.DiscordID = strings.TrimSpace(r.PostFormValue("discord_id"))
	}
	if has("phone") {
		phone, err := notice.NormalPhone(r.PostFormValue("phone"))
		if err != nil {
			return p, err
		}
		p.Phone = phone
		p.KeepConsent(base)
	}
	for _, w := range notice.Weights {
		if !has("rule_" + string(w)) {
			continue
		}
		rules := notice.Rules{}
		for k, v := range p.Rules {
			if k != w {
				rules[k] = v
			}
		}
		if line := strings.TrimSpace(r.PostFormValue("rule_" + string(w))); line != "" {
			steps, err := notice.ParseRule(line)
			if err != nil {
				return p, err
			}
			rules[w] = steps
		}
		p.Rules = rules
		if len(rules) == 0 {
			p.Rules = nil
		}
	}
	if has("quiet_from") || has("quiet_to") {
		p.Quiet = nil
		if from, to := strings.TrimSpace(r.PostFormValue("quiet_from")), strings.TrimSpace(r.PostFormValue("quiet_to")); from != "" || to != "" {
			p.Quiet = &notice.Quiet{From: from, To: to, Zone: strings.TrimSpace(r.PostFormValue("quiet_zone"))}
		}
	}
	return p, p.Validate()
}

// personLine is a person as a page shows them: their rule for each weight
// written the way the form takes it, and what the form shows, which is
// blank for a weight left at its default.
type personLine struct {
	notice.Person
	Rules []ruleLine
	Form  map[string]string
	// Reach is every medium the person has an address for.
	Reach []medium
}

// medium is a medium and what a person calls it.
type medium struct {
	M    notice.Medium
	Name string
}

func mediumName(m notice.Medium) string {
	switch m {
	case notice.TeamsChat:
		return "Teams"
	case notice.SMS:
		return "SMS"
	case notice.Discord:
		return "Discord"
	case notice.Email:
		return "Email"
	}
	return string(m)
}

// ruleSay is a rule as a sentence: "Teams, then SMS after 5 min".
func ruleSay(steps []notice.RuleStep) string {
	if len(steps) == 0 {
		return "Nothing is sent"
	}
	var b strings.Builder
	for i, st := range steps {
		if i > 0 {
			b.WriteString(", then ")
		}
		b.WriteString(mediumName(st.Medium))
		if st.After > 0 {
			b.WriteString(" after " + strconv.Itoa(st.After) + " min")
		}
	}
	return b.String()
}

type ruleLine struct {
	Weight  notice.Weight
	Text    string
	Say     string
	Default bool
}

func linePerson(p notice.Person) personLine {
	l := personLine{Person: p, Form: map[string]string{}}
	for _, m := range notice.Media {
		if p.Has(m) {
			l.Reach = append(l.Reach, medium{M: m, Name: mediumName(m)})
		}
	}
	// The heaviest weight first: a page, a notify, a notice.
	for i := len(notice.Weights) - 1; i >= 0; i-- {
		w := notice.Weights[i]
		_, set := p.Rules[w]
		steps := p.Rules.For(w, p)
		text := notice.FormatRule(steps)
		l.Rules = append(l.Rules, ruleLine{Weight: w, Text: text, Say: ruleSay(steps), Default: !set})
		if set {
			l.Form[string(w)] = text
		}
	}
	return l
}

func (s *Server) formSavePerson(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.refused(w, r, notice.ErrInvalid)
		return
	}
	email, err := notice.NormalEmail(r.FormValue("email"))
	if s.refused(w, r, err) {
		return
	}
	base := notice.Person{Email: email, CreatedAt: time.Now()}
	if old, err := s.store.Person(r.Context(), email); err == nil {
		base = old
	}
	p, err := personFromForm(r, base)
	if s.refused(w, r, err) {
		return
	}
	if s.refused(w, r, s.store.SavePerson(r.Context(), p)) {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "person saved", r, "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "person.save", "person", email, p.Name)
	s.renderPeople(w, r, http.StatusOK, "", personName(email)+" saved.")
}

func (s *Server) formDeletePerson(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(r.PathValue("email"))
	s.directoryChanged(w, r, "person", email, s.store.DeletePerson(r.Context(), email))
}

func (s *Server) formSaveGroup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.refused(w, r, notice.ErrInvalid)
		return
	}
	g := notice.Group{Slug: slugFor(r), Name: strings.TrimSpace(r.FormValue("name"))}
	if !notice.ValidSlug(g.Slug) || g.Name == "" || len(g.Name) > 80 {
		s.renderPeople(w, r, http.StatusBadRequest, "a group needs a slug of lowercase words joined by hyphens and a name of at most 80 characters", "")
		return
	}
	var members []string
	for _, v := range r.Form["members"] {
		members = append(members, splitList(v)...)
	}
	for _, m := range members {
		email, err := notice.NormalEmail(m)
		if s.refused(w, r, err) {
			return
		}
		if !slices.Contains(g.Members, email) {
			g.Members = append(g.Members, email)
		}
	}
	if len(g.Members) == 0 || len(g.Members) > 200 {
		s.renderPeople(w, r, http.StatusBadRequest, "a group has 1 to 200 members", "")
		return
	}
	if s.refused(w, r, s.store.SaveGroup(r.Context(), g)) {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "group saved", r, "group", g.Slug, "members", len(g.Members), "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "group.save", "group", g.Slug, g.Name)
	s.renderPeople(w, r, http.StatusOK, "", "Group "+g.Name+" saved.")
}

// slugFor is a form's slug, or one made from its name when the form has
// none: lowercase words joined by hyphens.
func slugFor(r *http.Request) string {
	if s := strings.TrimSpace(r.FormValue("slug")); s != "" {
		return s
	}
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(r.FormValue("name")), func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < '0' || c > '9')
	}) {
		words = append(words, w)
	}
	return strings.Join(words, "-")
}

func (s *Server) formDeleteGroup(w http.ResponseWriter, r *http.Request) {
	s.directoryChanged(w, r, "group", r.PathValue("slug"), s.store.DeleteGroup(r.Context(), r.PathValue("slug")))
}

func (s *Server) formSaveChain(w http.ResponseWriter, r *http.Request) {
	steps, err := notice.ParseSteps(r.FormValue("steps"))
	if s.refused(w, r, err) {
		return
	}
	repeat, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("repeat")))
	c := notice.Chain{Slug: slugFor(r), Name: strings.TrimSpace(r.FormValue("name")), Steps: steps, Repeat: repeat}
	if s.refused(w, r, c.Validate()) {
		return
	}
	for _, step := range c.Steps {
		for _, t := range step.Targets {
			if t.Group != "" {
				if _, err := s.store.Group(r.Context(), t.Group); errors.Is(err, store.ErrNotFound) {
					s.renderPeople(w, r, http.StatusBadRequest, "there is no group "+t.Group, "")
					return
				}
			}
			if t.Schedule != "" {
				if _, err := s.store.Schedule(r.Context(), t.Schedule, time.Now()); errors.Is(err, store.ErrNotFound) {
					s.renderPeople(w, r, http.StatusBadRequest, "there is no schedule "+t.Schedule, "")
					return
				}
			}
		}
	}
	if s.refused(w, r, s.store.SaveChain(r.Context(), c)) {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "chain saved", r, "chain", c.Slug, "steps", len(c.Steps), "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "chain.save", "chain", c.Slug, c.Name)
	s.renderPeople(w, r, http.StatusOK, "", "Chain "+c.Name+" saved.")
}

func (s *Server) formDeleteChain(w http.ResponseWriter, r *http.Request) {
	s.directoryChanged(w, r, "chain", r.PathValue("slug"), s.store.DeleteChain(r.Context(), r.PathValue("slug")))
}

// directoryChanged goes back to the People page after a removal.
func (s *Server) directoryChanged(w http.ResponseWriter, r *http.Request, what, id string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "People", "There is no such "+what+".")
		return
	}
	if s.pageFailed(w, r, err, "People") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, what+" removed", r, "by", door.From(r.Context()).Email)
	// A person is named only in person fields, never in a title.
	title := id
	if what == "person" {
		title = ""
	}
	s.decideAdmin(r, what+".remove", what, id, title)
	http.Redirect(w, r, "/people", http.StatusSeeOther)
}

func (s *Server) pageMe(w http.ResponseWriter, r *http.Request) {
	done := ""
	if r.URL.Query().Has("saved") {
		done = "Saved."
	}
	s.renderMe(w, r, http.StatusOK, "", done)
}

// renderMe shows a person their list and how they are reached.
func (s *Server) renderMe(w http.ResponseWriter, r *http.Request, status int, bad, done string) {
	ctx := r.Context()
	email := strings.ToLower(door.From(ctx).Email)
	p, err := s.store.Person(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		p, err = notice.Person{Email: email}, nil
	}
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	inbox, err := s.store.Inbox(ctx, email, 50)
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	lines := make([]inboxLine, 0, len(inbox))
	for _, l := range inbox {
		lines = append(lines, lineInbox(l))
	}
	grid, err := s.grid(r, p)
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	data := map[string]any{"Me": meLine{personLine: linePerson(p), OptIn: s.optInWords()}, "Inbox": lines, "Kinds": grid, "Bad": bad, "Done": done}
	if s.onDesk(r) {
		// Shifts belong to the desk; someone with no desk role sees none.
		data["Shifts"] = s.myShifts(r, email)
	}
	s.render(w, r, status, "me", data)
}

// inboxLine is an event in a person's list with what happened apart from
// what it happened to: "Cleared" and the incident's own title.
type inboxLine struct {
	store.InboxLine
	What, Title string
}

func lineInbox(l store.InboxLine) inboxLine {
	what := l.Event.Kind
	if i := strings.LastIndex(what, "."); i >= 0 {
		what = what[i+1:]
	}
	what = strings.ReplaceAll(what, "_", " ")
	return inboxLine{InboxLine: l, What: capitalize(what), Title: strings.TrimSuffix(l.Event.Title, " "+what)}
}

// formSaveMe saves the signed-in person's own settings. Nobody else's can
// be changed here: the address is the front door's.
func (s *Server) formSaveMe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderMe(w, r, http.StatusBadRequest, "the form could not be read", "")
		return
	}
	email, err := notice.NormalEmail(door.From(r.Context()).Email)
	if err != nil {
		s.renderMe(w, r, http.StatusBadRequest, "the front door gave no address", "")
		return
	}
	base := notice.Person{Email: email, CreatedAt: time.Now()}
	if old, err := s.store.Person(r.Context(), email); err == nil {
		base = old
	}
	p, err := personFromForm(r, base)
	if err == nil {
		err = optIn(r, &p, time.Now())
	}
	if errors.Is(err, notice.ErrInvalid) {
		s.renderMe(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": "), "")
		return
	}
	if s.pageFailed(w, r, s.store.SavePerson(r.Context(), p), "Me") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "person saved their own settings", r, "sms_consent", p.SMSConsentAt != nil)
	s.decideAdmin(r, "person.save", "person", email, p.Name)
	http.Redirect(w, r, "/me?saved", http.StatusSeeOther)
}

// meLine is the person on their own Me page, with the words they agree to
// before they are texted.
type meLine struct {
	personLine
	OptIn optInWords
}

// optInWords name who texts and where its SMS policy is, from the
// settings: dispatch names no estate of its own.
type optInWords struct {
	Sender string
	Policy string
}

func (s *Server) optInWords() optInWords {
	w := optInWords{Sender: s.cfg.SMSSenderName, Policy: s.cfg.SMSPolicyURL}
	if w.Sender == "" {
		w.Sender = "this desk"
	}
	return w
}

// optIn keeps when a person agreed to be texted. A Me form that saves a
// number must carry the ticked box; one that clears it needs none.
func optIn(r *http.Request, p *notice.Person, now time.Time) error {
	if _, ok := r.PostForm["phone"]; !ok || p.Phone == "" {
		return nil
	}
	if r.PostFormValue("sms_consent") == "" {
		return fmt.Errorf("%w: tick \"Text me alerts at this number\" to save a number; clearing it needs no box", notice.ErrInvalid)
	}
	at := now.UTC()
	p.SMSConsentAt = &at
	return nil
}

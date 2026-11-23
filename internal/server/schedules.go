package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// scheduleLine is a schedule as the page shows it: who is on call now,
// the next two weeks of shifts, its gaps, and its layers as the form
// takes them.
type scheduleLine struct {
	notice.Schedule
	Now string
	// Until is when the person on call now hands over, zero when nobody is.
	Until  time.Time
	Shifts []notice.Shift
	Gaps   []notice.Shift
	Local  *time.Location
	// From and To are the ribbon's ends: now, and two weeks on. A shift
	// clipped to either is said so.
	From, To time.Time
	// Blocks and Days draw the next two weeks as one ribbon.
	Blocks []shiftBlock
	Days   []dayTick
	// People is everyone with a shift in the two weeks, in their ribbon color.
	People []shiftBlock
	// Next is the shift after the one on now, when there is one.
	Next *notice.Shift
	// Form is the layers as the form's rows, with one blank row to add a
	// layer while there is room.
	Form layerForm
}

// layerForm is a schedule's layer rows and everyone a member pick may name.
type layerForm struct {
	Rows   []layerRow
	People []string
}

// Place is the schedule's zone as the place it names.
func (l scheduleLine) Place() string {
	for _, z := range zoneChoices(l.Zone) {
		if z.Zone == l.Zone {
			return z.Place
		}
	}
	return l.Zone
}

// Zones are the time zone picks for this schedule.
func (l scheduleLine) Zones() []zoneChoice { return zoneChoices(l.Zone) }

// layerRow is one layer as the schedule form's row: its sentence, and the
// fields its Edit opens. Members are in turn order, each its own pick with
// one blank pick after them; the first turn's start is a datetime-local
// value in the schedule's zone; hours are a from, a to, and the days.
type layerRow struct {
	Index int
	// Kept says the row is a layer the schedule has, not the blank one.
	Kept              bool
	Name, Start, Turn string
	Members, Slots    []string
	// Turns are the turn picks, the row's own among them.
	Turns []string
	// From and To are the hours, "" when the layer takes the whole day;
	// Days marks the weekdays it keeps to, Sunday first.
	From, To string
	DayPicks []dayPick
	// Said is the layer as words: when its turns start, how long each is,
	// and the hours it keeps to.
	TurnSaid, StartSaid, HoursSaid string
}

// dayPick is one weekday box of a layer's hours.
type dayPick struct {
	I    int
	Name string
	On   bool
}

// weekdayNames are the hours' days as ParseHours reads them, Sunday first.
var weekdayNames = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// turnSaid is a turn's length in words.
func turnSaid(d time.Duration) string {
	switch {
	case d == 7*24*time.Hour:
		return "a week"
	case d%(7*24*time.Hour) == 0:
		return fmt.Sprintf("%d weeks", d/(7*24*time.Hour))
	case d == 24*time.Hour:
		return "a day"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return fmt.Sprintf("%d hours", d/time.Hour)
}

// maxLayers is how many layers a schedule holds; notice.Schedule.Validate
// refuses more.
const maxLayers = 5

// turnPicks are the turns the form offers.
var turnPicks = []string{"12h", "1d", "1w", "2w"}

// layerRows is a schedule's layers as form rows.
func layerRows(layers []notice.Layer, loc *time.Location, now time.Time) []layerRow {
	out := make([]layerRow, 0, len(layers)+1)
	for k, l := range layers {
		row := layerRow{Index: k, Kept: true, Name: l.Name, Start: l.Start.In(loc).Format("2006-01-02T15:04"), Turn: notice.LengthString(l.Length),
			Members: l.Members, Slots: append(append([]string{}, l.Members...), ""), TurnSaid: turnSaid(l.Length), StartSaid: l.Start.In(loc).Format("Mon Jan 2, 15:04")}
		on := [7]bool{}
		if h := l.Hours; h != nil {
			row.From, row.To = h.From, h.To
			row.HoursSaid = h.From + " to " + h.To
			if len(h.Days) > 0 {
				var days []string
				for _, d := range h.Days {
					on[d] = true
					days = append(days, capitalize(weekdayNames[d]))
				}
				row.HoursSaid += " on " + strings.Join(days, ", ")
			}
		}
		row.DayPicks = dayPicks(on)
		out = append(out, row)
	}
	if len(out) < maxLayers {
		day := now.In(loc)
		start := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, loc)
		out = append(out, layerRow{Index: len(out), Start: start.Format("2006-01-02T15:04"), Turn: "1w", Slots: []string{""}, DayPicks: dayPicks([7]bool{})})
	}
	for k := range out {
		out[k].Turns = turnPicks
		if !slices.Contains(turnPicks, out[k].Turn) {
			out[k].Turns = append(append([]string{}, turnPicks...), out[k].Turn)
		}
	}
	return out
}

func dayPicks(on [7]bool) []dayPick {
	out := make([]dayPick, 7)
	for d := range out {
		out[d] = dayPick{I: d, Name: capitalize(weekdayNames[d]), On: on[d]}
	}
	return out
}

// rowHours reads a row's hours: a from and a to, and the days it keeps
// to when some but not all are ticked. No hours is the whole day.
func rowHours(r *http.Request, p string) string {
	from, to := strings.TrimSpace(r.FormValue(p+"from")), strings.TrimSpace(r.FormValue(p+"to"))
	if from == "" && to == "" {
		return r.FormValue(p + "hours")
	}
	h := from + "-" + to
	var days []string
	for _, d := range r.Form[p+"day"] {
		if n, err := strconv.Atoi(d); err == nil && n >= 0 && n < 7 {
			days = append(days, weekdayNames[n])
		}
	}
	if len(days) > 0 && len(days) < 7 {
		h += " " + strings.Join(days, ",")
	}
	return h
}

// layersFromRows reads the schedule form's rows, in order. A row marked
// to drop is left out, and so is a new row with nobody in it.
func layersFromRows(r *http.Request, loc *time.Location) ([]notice.Layer, error) {
	var out []notice.Layer
	for k := 0; k < maxLayers+1; k++ {
		p := fmt.Sprintf("l%d_", k)
		if r.FormValue(p+"drop") == "on" {
			continue
		}
		var members []string
		for _, m := range r.Form[p+"member"] {
			if m = strings.TrimSpace(m); m != "" && !slices.Contains(members, m) {
				members = append(members, m)
			}
		}
		for _, m := range strings.FieldsFunc(r.FormValue(p+"new"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' }) {
			if !slices.Contains(members, m) {
				members = append(members, m)
			}
		}
		if len(members) == 0 {
			// A layer the schedule had needs someone; a new row left empty is blank.
			if r.FormValue(p+"kept") == "1" {
				return nil, fmt.Errorf("%w: layer %d needs someone in it, or remove it", notice.ErrInvalid, k+1)
			}
			continue
		}
		layer, err := notice.ParseLayer(r.FormValue(p+"name"), strings.Join(members, ","), strings.Replace(r.FormValue(p+"start"), "T", " ", 1), r.FormValue(p+"turn"), rowHours(r, p), loc)
		if err != nil {
			return nil, fmt.Errorf("%w: layer %d: %s", notice.ErrInvalid, k+1, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": "))
		}
		out = append(out, layer)
	}
	return out, nil
}

// shiftBlock is one shift placed on the ribbon, in percent of its width.
type shiftBlock struct {
	Person      string
	Left, Width float64
	// Hue picks the person's color: the same person, the same color.
	Hue      int
	From, To time.Time
	// Label says the block is wide enough to carry the person's name.
	Label bool
}

// dayTick is one day's mark on the ribbon.
type dayTick struct {
	Label string
	Left  float64
	Today bool
}

// ribbonSpan is how far ahead the ribbon reaches.
const ribbonSpan = 14 * 24 * time.Hour

// ribbon places shifts and day marks on a ribbon that starts now.
// ribbon places shifts on the ribbon in each person's color from hues, which
// every schedule on the page shares; a person hues lacks is given the next.
func ribbon(shifts []notice.Shift, now time.Time, loc *time.Location, hues map[string]int) ([]shiftBlock, []dayTick, []shiftBlock) {
	end := now.Add(ribbonSpan)
	pos := func(t time.Time) float64 {
		return float64(t.Sub(now)) / float64(ribbonSpan) * 100
	}
	seen := map[string]bool{}
	var blocks, people []shiftBlock
	for _, sh := range shifts {
		from, to := sh.From, sh.To
		if from.Before(now) {
			from = now
		}
		if to.After(end) {
			to = end
		}
		if !to.After(from) {
			continue
		}
		b := shiftBlock{Person: sh.Person, Left: pos(from), Width: pos(to) - pos(from), From: sh.From, To: sh.To, Hue: -1}
		b.Label = b.Width >= 8
		if sh.Person != "" {
			h, ok := hues[sh.Person]
			if !ok {
				h = len(hues) % 6
				hues[sh.Person] = h
			}
			if !seen[sh.Person] {
				seen[sh.Person] = true
				people = append(people, shiftBlock{Person: sh.Person, Hue: h})
			}
			b.Hue = h
		}
		blocks = append(blocks, b)
	}
	var days []dayTick
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	for d := day.AddDate(0, 0, 1); d.Before(end); d = d.AddDate(0, 0, 1) {
		// A day that starts right after now would crowd the "now" label.
		if pos(d) < 2 || pos(d) > 93 {
			continue
		}
		// The label sits in the middle of its day.
		days = append(days, dayTick{Label: d.Format("Mon 2"), Left: pos(d.Add(12 * time.Hour))})
	}
	return blocks, days, people
}

func lineSchedule(sc notice.Schedule, now time.Time, hues map[string]int) scheduleLine {
	loc, err := time.LoadLocation(sc.Zone)
	if err != nil {
		loc = time.UTC
	}
	l := scheduleLine{Schedule: sc, Now: sc.OnCall(now), Shifts: sc.Shifts(now, now.Add(14*24*time.Hour)), Local: loc}
	l.Gaps = sc.Gaps(now, now.Add(14*24*time.Hour))
	l.Blocks, l.Days, l.People = ribbon(l.Shifts, now, loc, hues)
	l.From, l.To = now, now.Add(ribbonSpan)
	// A shift that runs to the ribbon's end has no handoff in sight: the
	// person holds every turn the two weeks show.
	if len(l.Shifts) > 0 && l.Shifts[0].Person != "" && l.Shifts[0].To.Before(l.To) {
		l.Until = l.Shifts[0].To
	}
	if len(l.Shifts) > 1 {
		l.Next = &l.Shifts[1]
	}
	l.Form.Rows = layerRows(sc.Layers, loc, now)
	return l
}

func (s *Server) pageSchedules(w http.ResponseWriter, r *http.Request) {
	s.renderSchedules(w, r, http.StatusOK, "", scheduleDone[r.URL.Query().Get("done")])
}

// scheduleDone is what a change on the Schedules page says once it lands,
// by the done key its redirect carries.
var scheduleDone = map[string]string{
	"saved": "Schedule saved.", "removed": "Schedule removed.", "covered": "Cover added.", "uncovered": "Cover removed.",
}

// scheduleBack sends a Schedules change back to the page, to the schedule
// it changed.
func scheduleBack(w http.ResponseWriter, r *http.Request, done, slug string) {
	to := "/schedules?done=" + done
	if slug != "" {
		to += "#" + slug
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) renderSchedules(w http.ResponseWriter, r *http.Request, status int, bad, done string) {
	now := time.Now()
	schedules, err := s.store.Schedules(r.Context(), now)
	if s.pageFailed(w, r, err, "Schedules") {
		return
	}
	lines := make([]scheduleLine, 0, len(schedules))
	hues := scheduleHues(schedules)
	for _, sc := range schedules {
		lines = append(lines, lineSchedule(sc, now, hues))
	}
	me := door.From(r.Context()).Email
	people, err := s.store.People(r.Context())
	if s.pageFailed(w, r, err, "Schedules") {
		return
	}
	// Everyone a pick may name: the directory, everyone on a schedule, and you.
	seen := map[string]bool{}
	var picks []string
	pick := func(e string) {
		if e != "" && !seen[strings.ToLower(e)] {
			seen[strings.ToLower(e)] = true
			picks = append(picks, e)
		}
	}
	pick(me)
	for _, p := range people {
		pick(p.Email)
	}
	for _, sc := range schedules {
		for _, l := range sc.Layers {
			for _, m := range l.Members {
				pick(m)
			}
		}
	}
	sort.SliceStable(picks, func(a, b int) bool { return personName(picks[a]) < personName(picks[b]) })
	for k := range lines {
		lines[k].Form.People = picks
	}
	s.render(w, r, status, "schedules", map[string]any{"Schedules": lines, "Bad": bad, "Done": done, "Me": me, "People": picks,
		"NewZones": zoneChoices(""), "Blank": layerForm{Rows: layerRows(nil, time.UTC, now), People: picks}, "Turns": []string{"12h", "1d", "1w", "2w"}})
}

// scheduleRefused shows a refused Schedules form, and anything else as a
// failure.
func (s *Server) scheduleRefused(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, notice.ErrInvalid) {
		s.renderSchedules(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": "), "")
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		s.renderSchedules(w, r, http.StatusNotFound, "there is no such schedule", "")
		return true
	}
	return s.pageFailed(w, r, err, "Schedules")
}

// formSaveSchedule adds or replaces a schedule. Its layers are one a line:
// name | members | first turn's start | turn | hours.
func (s *Server) formSaveSchedule(w http.ResponseWriter, r *http.Request) {
	sc := notice.Schedule{Slug: strings.TrimSpace(r.FormValue("slug")), Name: strings.TrimSpace(r.FormValue("name")), Zone: strings.TrimSpace(r.FormValue("zone"))}
	if sc.Slug == "" {
		sc.Slug = slugOf(sc.Name)
	}
	loc, err := time.LoadLocation(sc.Zone)
	if err != nil || sc.Zone == "" {
		s.renderSchedules(w, r, http.StatusBadRequest, fmt.Sprintf("%q is not a time zone such as America/New_York", sc.Zone), "")
		return
	}
	if r.FormValue("rows") == "1" {
		layers, err := layersFromRows(r, loc)
		if err != nil {
			s.renderSchedules(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": "), "")
			return
		}
		sc.Layers = layers
	}
	for n, line := range strings.Split(r.FormValue("layers"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		for len(parts) < 5 {
			parts = append(parts, "")
		}
		layer, err := notice.ParseLayer(parts[0], parts[1], parts[2], parts[3], parts[4], loc)
		if err != nil {
			s.renderSchedules(w, r, http.StatusBadRequest, fmt.Sprintf("layer %d: %s", n+1, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": ")), "")
			return
		}
		sc.Layers = append(sc.Layers, layer)
	}
	if s.scheduleRefused(w, r, s.desk.SaveSchedule(r.Context(), sc)) {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "schedule saved", r, "schedule", sc.Slug, "layers", len(sc.Layers), "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "schedule.save", "schedule", sc.Slug, sc.Name)
	scheduleBack(w, r, "saved", sc.Slug)
}

func (s *Server) formDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if s.scheduleRefused(w, r, s.store.DeleteSchedule(r.Context(), slug)) {
		return
	}
	s.decideAdmin(r, "schedule.remove", "schedule", slug, "")
	scheduleBack(w, r, "removed", "")
}

// formCover puts someone on call for a while; anyone on the desk may, for
// themselves or another, and Annals records who did.
func (s *Server) formCover(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	offset, _ := strconv.Atoi(r.FormValue("tz_offset"))
	zone := time.FixedZone("browser", -offset*60)
	from, err1 := time.ParseInLocation("2006-01-02T15:04", r.FormValue("from"), zone)
	to, err2 := time.ParseInLocation("2006-01-02T15:04", r.FormValue("to"), zone)
	if err1 != nil || err2 != nil {
		s.renderSchedules(w, r, http.StatusBadRequest, "a cover needs a start and an end", "")
		return
	}
	by := door.From(r.Context()).Email
	person := strings.TrimSpace(r.FormValue("person"))
	if person == "" {
		person = by
	}
	o, err := s.desk.Cover(r.Context(), slug, notice.Override{Person: person, From: from.UTC(), To: to.UTC(), By: by, Reason: strings.TrimSpace(r.FormValue("reason"))})
	if s.scheduleRefused(w, r, err) {
		return
	}
	s.decideAdmin(r, "schedule.cover", "schedule", slug, "", annals.PersonParty("cover", o.Person))
	scheduleBack(w, r, "covered", slug)
}

func (s *Server) formDeleteCover(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.store.DeleteOverride(r.Context(), slug, id)
	}
	if s.scheduleRefused(w, r, err) {
		return
	}
	s.decideAdmin(r, "schedule.uncover", "schedule", slug, r.PathValue("id"))
	scheduleBack(w, r, "uncovered", slug)
}

// formCalendar makes the signed-in person a new calendar link, shown once.
func (s *Server) formCalendar(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(door.From(r.Context()).Email)
	if err := s.store.LearnPerson(r.Context(), email, time.Now()); err != nil && s.pageFailed(w, r, err, "Me") {
		return
	}
	secret, err := s.store.NewCalendar(r.Context(), email)
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "calendar link made", r)
	s.decideAdmin(r, "person.calendar", "person", email, "")
	s.renderMe(w, r, http.StatusOK, "", "Your calendar link, shown only now; making another stops this one: "+s.cfg.PublicURL+"/v1/calendar/"+secret+".ics")
}

// myShifts is a person's shifts in the next two weeks on every schedule.
func (s *Server) myShifts(r *http.Request, email string) []shiftLine {
	now := time.Now()
	schedules, err := s.store.Schedules(r.Context(), now)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "schedules not read", r, "error", err)
		return nil
	}
	var out []shiftLine
	for _, sc := range schedules {
		for _, sh := range sc.Shifts(now, now.Add(14*24*time.Hour)) {
			if sh.Person == email {
				out = append(out, shiftLine{Shift: sh, Schedule: sc.Name})
			}
		}
	}
	return out
}

type shiftLine struct {
	notice.Shift
	Schedule string
}

// apiCalendar is a person's shifts as an iCalendar feed. Calendar apps
// cannot send a bearer token, so the link's secret is the credential: it
// is checked against the hash kept for one person, and the feed holds
// only that person's shifts.
func (s *Server) apiCalendar(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimSuffix(r.PathValue("secret"), ".ics")
	email, err := s.store.CalendarOwner(r.Context(), secret)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelWarn, "calendar refused: unknown link", r)
		writeError(w, http.StatusNotFound, "no such calendar")
		return
	}
	s.logRequest(r.Context(), slog.LevelDebug, "calendar admitted", r)
	now := time.Now()
	schedules, err := s.store.Schedules(r.Context(), now.Add(-7*24*time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the schedules could not be read")
		return
	}
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//dispatch//on call//EN\r\nX-WR-CALNAME:On call\r\n")
	stamp := now.UTC().Format("20060102T150405Z")
	for _, sc := range schedules {
		for _, sh := range sc.Shifts(now.Add(-7*24*time.Hour), now.Add(60*24*time.Hour)) {
			if sh.Person != email {
				continue
			}
			fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:%s-%d@dispatch\r\nDTSTAMP:%s\r\nDTSTART:%s\r\nDTEND:%s\r\nSUMMARY:On call: %s\r\nURL:%s/schedules\r\nEND:VEVENT\r\n",
				sc.Slug, sh.From.Unix(), stamp, sh.From.UTC().Format("20060102T150405Z"), sh.To.UTC().Format("20060102T150405Z"), icalText(sc.Name), s.cfg.PublicURL)
		}
	}
	b.WriteString("END:VCALENDAR\r\n")
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

func icalText(s string) string {
	return strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\n", "\\n", "\r", "").Replace(s)
}

// apiOnCall lists every schedule with who is on call now and until when.
func (s *Server) apiOnCall(w http.ResponseWriter, r *http.Request, _ caller) {
	now := time.Now()
	schedules, err := s.store.Schedules(r.Context(), now)
	if s.failed(w, r, err, "schedules") {
		return
	}
	type oncall struct {
		Slug   string    `json:"slug"`
		Name   string    `json:"name"`
		Person string    `json:"person"`
		Until  time.Time `json:"until"`
	}
	out := []oncall{}
	for _, sc := range schedules {
		out = append(out, oncall{Slug: sc.Slug, Name: sc.Name, Person: sc.OnCall(now), Until: sc.Until(now)})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiCover puts someone on call over the API, as the token.
func (s *Server) apiCover(w http.ResponseWriter, r *http.Request, by caller) {
	var o notice.Override
	if err := decodeJSON(w, r, &o); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be JSON with person, from, and to")
		return
	}
	o.ID, o.By = 0, string(by)
	o, err := s.desk.Cover(r.Context(), r.PathValue("slug"), o)
	switch {
	case errors.Is(err, notice.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such schedule")
		return
	case s.failed(w, r, err, "cover"):
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

// formAgentToken makes the signed-in person a token that acts as them,
// for their own agents, shown once.
func (s *Server) formAgentToken(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(door.From(r.Context()).Email)
	tok, secret, err := s.store.MintPersonToken(r.Context(), "agent of "+email, email)
	if s.pageFailed(w, r, err, "Me") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "personal token made", r, "token", tok.ID)
	s.decideAdmin(r, "token.issue", "token", tok.Name, tok.Name)
	s.renderMe(w, r, http.StatusOK, "", "A token that acts as you, shown only now: "+secret+". Point an MCP client at "+s.cfg.PublicURL+"/mcp with it as a bearer token, or use it on the API. Revoke it on Settings, Machines.")
}

// scheduleHues gives everyone on any schedule their own color, in the order
// of their addresses, so a person looks the same on every ribbon.
func scheduleHues(schedules []notice.Schedule) map[string]int {
	var people []string
	seen := map[string]bool{}
	for _, sc := range schedules {
		for _, layer := range sc.Layers {
			for _, m := range layer.Members {
				if !seen[m] {
					seen[m] = true
					people = append(people, m)
				}
			}
		}
	}
	sort.Strings(people)
	hues := map[string]int{}
	for i, p := range people {
		hues[p] = i % 6
	}
	return hues
}

// slugOf makes a schedule's slug from its name: lowercase words joined by
// hyphens, as chains name it.
func slugOf(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// zonePicks are the time zones the schedule form offers; a schedule's own
// zone is offered too when it is not one of them.
var zonePicks = []string{
	"UTC", "America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage",
	"Pacific/Honolulu", "America/Toronto", "America/Mexico_City", "America/Sao_Paulo", "Europe/London", "Europe/Dublin", "Europe/Lisbon",
	"Europe/Paris", "Europe/Berlin", "Europe/Amsterdam", "Europe/Madrid", "Europe/Stockholm", "Europe/Helsinki", "Africa/Johannesburg",
	"Asia/Dubai", "Asia/Kolkata", "Asia/Singapore", "Asia/Shanghai", "Asia/Tokyo", "Australia/Sydney", "Pacific/Auckland",
}

// zoneChoice is one time zone pick: its value and the place it names.
type zoneChoice struct{ Zone, Place string }

// zoneChoices are the picks for a schedule in zone, its own first when
// the list lacks it.
func zoneChoices(zone string) []zoneChoice {
	list := zonePicks
	if zone != "" && !slices.Contains(list, zone) {
		list = append([]string{zone}, list...)
	}
	out := make([]zoneChoice, 0, len(list))
	for _, z := range list {
		place := z
		if i := strings.LastIndex(z, "/"); i >= 0 {
			place = strings.ReplaceAll(z[i+1:], "_", " ") + " (" + z[:strings.Index(z, "/")] + ")"
		}
		out = append(out, zoneChoice{Zone: z, Place: place})
	}
	return out
}

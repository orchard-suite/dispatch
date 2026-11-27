// Package config is the desk's routing as one YAML document kept in git:
// people, groups, chains, schedules, subscriptions, heartbeats, and the
// settings, with how changes are read. It reads and writes the document and checks it; applying it
// is the desk's.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"
	"gopkg.in/yaml.v3"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/signals"
)

// MaxBody is the most of a document the desk reads.
const MaxBody = 1 << 20

// Document is the whole of the desk's routing. A section left out is left
// as it is; a section given replaces what the desk has, except people,
// who are added or changed but never removed, since everyone who signs in
// is one.
type Document struct {
	Settings      *Settings      `yaml:"settings,omitempty"`
	People        []Person       `yaml:"people,omitempty"`
	Groups        []notice.Group `yaml:"groups,omitempty"`
	Chains        []Chain        `yaml:"chains,omitempty"`
	Schedules     []Schedule     `yaml:"schedules,omitempty"`
	Subscriptions []Subscription `yaml:"subscriptions,omitempty"`
	Heartbeats    []Heartbeat    `yaml:"heartbeats,omitempty"`
}

// Settings is the part of the desk's settings a document holds.
type Settings struct {
	GroupingMinutes int  `yaml:"grouping_minutes"`
	AutoResolveInfo bool `yaml:"auto_resolve_info"`
	// QuietResolveHours resolves an incident once its alerts have been
	// quiet this many hours; 0 turns it off.
	QuietResolveHours int               `yaml:"quiet_resolve_hours"`
	Rules             string            `yaml:"rules,omitempty"`
	Routes            string            `yaml:"routes,omitempty"`
	StatusTitle       string            `yaml:"status_title,omitempty"`
	Public            map[string]string `yaml:"public_components,omitempty"`
	Templates         map[string]string `yaml:"update_templates,omitempty"`
	IssueRepos        map[string]string `yaml:"issue_repos,omitempty"`
	Zone              string            `yaml:"zone,omitempty"`
	PersonPages       int               `yaml:"person_pages,omitempty"`
	ServicePages      int               `yaml:"service_pages,omitempty"`
	Changes           *Changes          `yaml:"changes,omitempty"`
}

// Changes is how the desk reads changes: which catalog relations say a
// thing leans on another, how many hops to follow them, and the estate's
// effect kinds.
type Changes struct {
	LeansOn []string `yaml:"leans_on,omitempty"`
	Depth   int      `yaml:"depth,omitempty"`
	// Kinds are the estate's own effect kinds, and base kinds whose watch
	// it changes. A base kind keeps its family and unit; only its watch
	// may differ.
	Kinds []Kind `yaml:"kinds,omitempty"`
	// Watch is where the watch reads signals, and which query reads which
	// signal for which blueprint.
	Watch *signals.Settings `yaml:"watch,omitempty"`
}

// Kind is an effect kind with its watch written as a duration, such as 2h.
type Kind struct {
	Name   string `yaml:"name"`
	Family string `yaml:"family"`
	Means  string `yaml:"means"`
	Unit   string `yaml:"unit,omitempty"`
	Watch  string `yaml:"watch,omitempty"`
}

// ToKinds is a document's kinds as the kit's, checked over the base set.
func (c Changes) ToKinds() ([]change.Kind, *change.Kinds, error) {
	out := make([]change.Kind, 0, len(c.Kinds))
	for _, k := range c.Kinds {
		kind := change.Kind{Name: k.Name, Family: k.Family, Means: k.Means, Unit: k.Unit}
		if k.Watch != "" {
			d, err := time.ParseDuration(k.Watch)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: kind %s: watch is a duration such as 30m", notice.ErrInvalid, k.Name)
			}
			kind.Watch = change.Duration(d)
		}
		out = append(out, kind)
	}
	reg, err := KindsOf(out)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", notice.ErrInvalid, err)
	}
	return out, reg, nil
}

// KindsOf is the base kinds with an estate's added.
func KindsOf(kinds []change.Kind) (*change.Kinds, error) {
	b, err := json.Marshal(change.KindsFile{Kinds: kinds})
	if err != nil {
		return nil, err
	}
	return change.LoadKinds(bytes.NewReader(b))
}

// FromKinds is the estate's kinds as a document's.
func FromKinds(kinds []change.Kind) []Kind {
	out := make([]Kind, 0, len(kinds))
	for _, k := range kinds {
		kind := Kind{Name: k.Name, Family: k.Family, Means: k.Means, Unit: k.Unit}
		if k.Watch > 0 {
			kind.Watch = time.Duration(k.Watch).String()
		}
		out = append(out, kind)
	}
	return out
}

// Person is a person with their rules written the way the forms take them.
type Person struct {
	Email     string            `yaml:"email"`
	Name      string            `yaml:"name,omitempty"`
	Phone     string            `yaml:"phone,omitempty"`
	DiscordID string            `yaml:"discord_id,omitempty"`
	Rules     map[string]string `yaml:"rules,omitempty"`
	Quiet     *notice.Quiet     `yaml:"quiet,omitempty"`
}

// Chain is a chain with its steps one a line, as the People page takes
// them.
type Chain struct {
	Slug   string `yaml:"slug"`
	Name   string `yaml:"name"`
	Steps  string `yaml:"steps"`
	Repeat int    `yaml:"repeat,omitempty"`
}

// Schedule is a schedule with its layers written as the Schedules page
// takes them.
type Schedule struct {
	Slug   string  `yaml:"slug"`
	Name   string  `yaml:"name"`
	Zone   string  `yaml:"zone"`
	Layers []Layer `yaml:"layers"`
}

// Layer is a layer: members, the first turn's start in the schedule's
// zone ("2006-01-02 15:04"), the turn ("1w"), and optional hours.
type Layer struct {
	Name    string   `yaml:"name,omitempty"`
	Members []string `yaml:"members"`
	Start   string   `yaml:"start"`
	Every   string   `yaml:"every"`
	Hours   string   `yaml:"hours,omitempty"`
}

// Subscription sends a channel, by name, or a target the events it takes.
type Subscription struct {
	Channel   string            `yaml:"channel,omitempty"`
	To        string            `yaml:"to,omitempty"`
	Topics    []string          `yaml:"topics"`
	Producers []string          `yaml:"producers,omitempty"`
	About     string            `yaml:"about,omitempty"`
	Match     map[string]string `yaml:"match,omitempty"`
	Paused    bool              `yaml:"paused,omitempty"`
}

// Heartbeat is a heartbeat with its durations as text, such as 24h.
type Heartbeat struct {
	Name     string `yaml:"name"`
	Every    string `yaml:"every"`
	Grace    string `yaml:"grace,omitempty"`
	Service  string `yaml:"service,omitempty"`
	Severity string `yaml:"severity,omitempty"`
}

// Read reads a document, refusing a field it does not know.
func Read(r io.Reader) (Document, error) {
	var d Document
	dec := yaml.NewDecoder(io.LimitReader(r, MaxBody))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("%w: the document is not dispatch's config: %v", notice.ErrInvalid, err)
	}
	return d, nil
}

// Write writes a document.
func Write(w io.Writer, d Document) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(d)
}

// ToPerson is a document's person as the desk's.
func (p Person) ToPerson() (notice.Person, error) {
	email, err := notice.NormalEmail(p.Email)
	if err != nil {
		return notice.Person{}, err
	}
	out := notice.Person{Email: email, Name: strings.TrimSpace(p.Name), DiscordID: p.DiscordID, Quiet: p.Quiet}
	if out.Phone, err = notice.NormalPhone(p.Phone); err != nil {
		return out, err
	}
	for w, line := range p.Rules {
		weight, err := notice.ParseWeight(w)
		if err != nil || weight == "" {
			return out, fmt.Errorf("%w: %s: rules are for page, notify, or notice", notice.ErrInvalid, email)
		}
		steps, err := notice.ParseRule(line)
		if err != nil {
			return out, err
		}
		if out.Rules == nil {
			out.Rules = notice.Rules{}
		}
		out.Rules[weight] = steps
	}
	return out, out.Validate()
}

// FromPerson is the desk's person as a document's.
func FromPerson(p notice.Person) Person {
	out := Person{Email: p.Email, Name: p.Name, Phone: p.Phone, DiscordID: p.DiscordID, Quiet: p.Quiet}
	for w, steps := range p.Rules {
		if out.Rules == nil {
			out.Rules = map[string]string{}
		}
		out.Rules[string(w)] = notice.FormatRule(steps)
	}
	return out
}

// ToChain is a document's chain as the desk's.
func (c Chain) ToChain() (notice.Chain, error) {
	steps, err := notice.ParseSteps(c.Steps)
	if err != nil {
		return notice.Chain{}, err
	}
	out := notice.Chain{Slug: c.Slug, Name: c.Name, Steps: steps, Repeat: c.Repeat}
	return out, out.Validate()
}

// ToSchedule is a document's schedule as the desk's.
func (s Schedule) ToSchedule() (notice.Schedule, error) {
	loc, err := time.LoadLocation(s.Zone)
	if err != nil || s.Zone == "" {
		return notice.Schedule{}, fmt.Errorf("%w: %s: %q is not a time zone", notice.ErrInvalid, s.Slug, s.Zone)
	}
	out := notice.Schedule{Slug: s.Slug, Name: s.Name, Zone: s.Zone}
	for _, l := range s.Layers {
		layer, err := notice.ParseLayer(l.Name, strings.Join(l.Members, ","), l.Start, l.Every, l.Hours, loc)
		if err != nil {
			return out, fmt.Errorf("%s: %w", s.Slug, err)
		}
		out.Layers = append(out.Layers, layer)
	}
	return out, out.Validate()
}

// FromSchedule is the desk's schedule as a document's.
func FromSchedule(s notice.Schedule) Schedule {
	loc, err := time.LoadLocation(s.Zone)
	if err != nil {
		loc = time.UTC
	}
	out := Schedule{Slug: s.Slug, Name: s.Name, Zone: s.Zone}
	for _, l := range s.Layers {
		out.Layers = append(out.Layers, Layer{Name: l.Name, Members: l.Members, Start: l.Start.In(loc).Format("2006-01-02 15:04"), Every: notice.LengthString(l.Length), Hours: l.Hours.String()})
	}
	return out
}

// ToHeartbeat is a document's heartbeat as the desk's.
func (h Heartbeat) ToHeartbeat() (incident.Heartbeat, error) {
	every, err := time.ParseDuration(h.Every)
	if err != nil {
		return incident.Heartbeat{}, fmt.Errorf("%w: %s: every is a duration such as 24h", notice.ErrInvalid, h.Name)
	}
	var grace time.Duration
	if h.Grace != "" {
		if grace, err = time.ParseDuration(h.Grace); err != nil {
			return incident.Heartbeat{}, fmt.Errorf("%w: %s: grace is a duration such as 30m", notice.ErrInvalid, h.Name)
		}
	}
	sev := incident.Severity(h.Severity)
	if sev == "" {
		sev = incident.Critical
	}
	out := incident.Heartbeat{Name: h.Name, Interval: every, Grace: grace, Service: h.Service, Severity: sev}
	if err := out.Validate(); err != nil {
		return out, fmt.Errorf("%w: %v", notice.ErrInvalid, err)
	}
	if incident.ParseSeverity(string(sev)) != sev {
		return out, fmt.Errorf("%w: %s: severity is critical, warning, or info", notice.ErrInvalid, h.Name)
	}
	return out, nil
}

package notice

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Target is where an event is addressed beyond the channels: one person by
// email, a group, an escalation chain, or whoever a schedule has on call,
// by slug. Exactly one is set.
type Target struct {
	Person   string `json:"person,omitempty"`
	Group    string `json:"group,omitempty"`
	Chain    string `json:"chain,omitempty"`
	Schedule string `json:"schedule,omitempty"`
}

// String is the target as the desk writes it: "person:a@example.com",
// "group:oncall", or "chain:platform".
func (t Target) String() string {
	switch {
	case t.Person != "":
		return "person:" + t.Person
	case t.Group != "":
		return "group:" + t.Group
	case t.Chain != "":
		return "chain:" + t.Chain
	case t.Schedule != "":
		return "schedule:" + t.Schedule
	}
	return ""
}

// ParseTarget reads "person:email", "group:slug", or "chain:slug". A bare
// address is a person.
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	kind, value, ok := strings.Cut(s, ":")
	if !ok {
		kind, value = "person", s
	}
	var t Target
	switch kind {
	case "person":
		t.Person = value
	case "group":
		t.Group = value
	case "chain":
		t.Chain = value
	case "schedule":
		t.Schedule = value
	default:
		return t, fmt.Errorf("%w: %q is not person:, group:, chain:, or schedule:", ErrInvalid, s)
	}
	return t.Normal()
}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidSlug says whether s names a group or a chain: lowercase words joined
// by hyphens, at most 64 characters.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// NormalEmail is an address as dispatch keeps it, lowercased, or an error
// when it is not one plain address.
func NormalEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, s)
	}
	return s, nil
}

// Normal checks the target and lowercases its address.
func (t Target) Normal() (Target, error) {
	set := 0
	for _, v := range []string{t.Person, t.Group, t.Chain, t.Schedule} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return t, fmt.Errorf("%w: a target is one of person, group, chain, or schedule", ErrInvalid)
	}
	if t.Person != "" {
		email, err := NormalEmail(t.Person)
		t.Person = email
		return t, err
	}
	if slug := t.Group + t.Chain + t.Schedule; !ValidSlug(slug) {
		return t, fmt.Errorf("%w: %q is not a slug", ErrInvalid, slug)
	}
	return t, nil
}

// Medium is how a person is reached: an app they already have.
type Medium string

const (
	Email     Medium = "email"
	TeamsChat Medium = "teams"
	SMS       Medium = "sms"
	Discord   Medium = "discord"
)

// Media are every medium, in the order a person is told.
var Media = []Medium{TeamsChat, Discord, SMS, Email}

// ParseMedium reads one medium's word.
func ParseMedium(s string) (Medium, bool) {
	for _, m := range Media {
		if string(m) == strings.TrimSpace(s) {
			return m, true
		}
	}
	return "", false
}

// Person is someone dispatch can reach, or an address such as a
// distribution list. Phone and DiscordID are the person's own SMS number
// and Discord account; TeamsID is their Teams account, learned from the
// rosters of the teams the bot is in. Rules say which media each weight
// takes and when; Quiet holds notify-weight messages back overnight.
// SMSConsentAt is when the person agreed on Me to be texted at Phone; it
// is nil for a number someone else added.
type Person struct {
	Email        string     `json:"email"`
	Name         string     `json:"name"`
	Phone        string     `json:"phone,omitempty"`
	SMSConsentAt *time.Time `json:"sms_consent_at,omitempty"`
	DiscordID    string     `json:"discord_id,omitempty"`
	Rules        Rules      `json:"rules,omitempty"`
	Quiet        *Quiet     `json:"quiet,omitempty"`
	TeamsID      string     `json:"-"`
	Learned      bool       `json:"learned"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Label is the person's name, or their address without one.
func (p Person) Label() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Email
}

// Has says whether the person has an address for m: email and Teams by
// their address, SMS with a phone, Discord with an account. Teams finds
// the account by address when it first sends.
func (p Person) Has(m Medium) bool {
	switch m {
	case Email, TeamsChat:
		return p.Email != ""
	case SMS:
		return p.Phone != ""
	case Discord:
		return p.DiscordID != ""
	}
	return false
}

// KeepConsent carries old's consent to texts onto p while the number is
// the same. A new or cleared number has no consent until its person gives it.
func (p *Person) KeepConsent(old Person) {
	p.SMSConsentAt = nil
	if p.Phone != "" && p.Phone == old.Phone {
		p.SMSConsentAt = old.SMSConsentAt
	}
}

var phoneRe = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// NormalPhone is a number as dispatch keeps it, E.164 such as +15555550100,
// with spaces, dashes, dots, and brackets dropped.
func NormalPhone(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(" -.()", r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !phoneRe.MatchString(s) {
		return "", fmt.Errorf("%w: %q is not a phone number in +country form, such as +15555550100", ErrInvalid, s)
	}
	return s, nil
}

var discordRe = regexp.MustCompile(`^[0-9]{15,21}$`)

// ValidDiscordID says whether s is a Discord account's numeric id, or empty.
func ValidDiscordID(s string) bool { return s == "" || discordRe.MatchString(s) }

// Validate checks a person's own settings.
func (p Person) Validate() error {
	if !ValidDiscordID(p.DiscordID) {
		return fmt.Errorf("%w: a Discord id is the account's number, 15 to 21 digits", ErrInvalid)
	}
	if strings.TrimSpace(p.Name) != p.Name || len(p.Name) > 80 {
		return fmt.Errorf("%w: a name is at most 80 characters", ErrInvalid)
	}
	if err := p.Rules.Validate(); err != nil {
		return err
	}
	if p.Quiet != nil {
		return p.Quiet.Validate()
	}
	return nil
}

// Group is a set of people told together.
type Group struct {
	Slug    string   `json:"slug"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// Step is one rung of a chain: who is told, and how long the chain waits
// for someone to acknowledge before it tells the next step.
type Step struct {
	Targets []Target `json:"targets"`
	Wait    int      `json:"wait_minutes"`
}

// Chain tells its steps in order until someone acknowledges, and then
// from the first step again Repeat more times.
type Chain struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Steps  []Step `json:"steps"`
	Repeat int    `json:"repeat,omitempty"`
}

// Validate checks a chain: a slug, a name, and one to ten steps of people
// and groups, each waiting 0 to 1440 minutes.
func (c Chain) Validate() error {
	switch {
	case !ValidSlug(c.Slug):
		return fmt.Errorf("%w: a chain needs a slug of lowercase words joined by hyphens", ErrInvalid)
	case strings.TrimSpace(c.Name) == "" || len(c.Name) > 80:
		return fmt.Errorf("%w: a chain needs a name of at most 80 characters", ErrInvalid)
	case len(c.Steps) == 0 || len(c.Steps) > 10:
		return fmt.Errorf("%w: a chain has 1 to 10 steps", ErrInvalid)
	case c.Repeat < 0 || c.Repeat > 5:
		return fmt.Errorf("%w: a chain repeats 0 to 5 times", ErrInvalid)
	}
	for i, s := range c.Steps {
		if len(s.Targets) == 0 || len(s.Targets) > 20 {
			return fmt.Errorf("%w: step %d needs 1 to 20 people, groups, or schedules", ErrInvalid, i+1)
		}
		if s.Wait < 0 || s.Wait > 1440 {
			return fmt.Errorf("%w: step %d waits 0 to 1440 minutes", ErrInvalid, i+1)
		}
		for _, t := range s.Targets {
			if t.Chain != "" {
				return fmt.Errorf("%w: a chain's step names people, groups, and schedules, not chains", ErrInvalid)
			}
		}
	}
	return nil
}

// ParseSteps reads a chain's steps, one a line: the wait in minutes, then
// the people and groups, such as "15 group:oncall person:a@example.com".
func ParseSteps(text string) ([]Step, error) {
	var steps []Step
	for n, line := range strings.Split(text, "\n") {
		words := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\r' })
		if len(words) == 0 {
			continue
		}
		wait, err := strconv.Atoi(words[0])
		if err != nil || len(words) < 2 {
			return nil, fmt.Errorf("%w: line %d is a wait in minutes, then people and groups", ErrInvalid, n+1)
		}
		s := Step{Wait: wait}
		for _, w := range words[1:] {
			t, err := ParseTarget(w)
			if err != nil {
				return nil, err
			}
			s.Targets = append(s.Targets, t)
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// FormatSteps writes steps the way ParseSteps reads them.
func FormatSteps(steps []Step) string {
	lines := make([]string, 0, len(steps))
	for _, s := range steps {
		words := []string{strconv.Itoa(s.Wait)}
		for _, t := range s.Targets {
			words = append(words, t.String())
		}
		lines = append(lines, strings.Join(words, " "))
	}
	return strings.Join(lines, "\n")
}

// PageState is where a page is.
type PageState string

const (
	PageActive       PageState = "active"
	PageAcknowledged PageState = "acknowledged"
	PageUnanswered   PageState = "unanswered"
)

// Page is one event working its way up a chain. Step is the step last
// told; NextAt is when the next is told if nobody acknowledges first.
// Round counts the times it has started over from the first step, up to
// Repeat.
type Page struct {
	ID             int64      `json:"id"`
	EventID        int64      `json:"event_id"`
	Chain          string     `json:"chain"`
	Steps          []Step     `json:"steps"`
	Step           int        `json:"step"`
	Round          int        `json:"round"`
	Repeat         int        `json:"repeat"`
	State          PageState  `json:"state"`
	NextAt         time.Time  `json:"next_at"`
	StartedAt      time.Time  `json:"started_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
}

// ErrUnreachable is a person a medium cannot reach, such as someone Teams
// does not know. It is not tried again.
var ErrUnreachable = errors.New("unreachable")

// Skipped is a message that could not be sent by its medium and is not
// tried again, such as a person Teams does not know.
const Skipped DeliveryState = "skipped"

// Message is one event on its way to one person by one medium. Page is the
// chain's page it was sent for, or zero.
type Message struct {
	ID        int64         `json:"id"`
	EventID   int64         `json:"event_id"`
	Person    string        `json:"person"`
	Medium    Medium        `json:"medium"`
	PageID    int64         `json:"page_id,omitempty"`
	State     DeliveryState `json:"state"`
	Attempts  int           `json:"attempts"`
	NextAt    time.Time     `json:"next_at"`
	Status    int           `json:"status,omitempty"`
	LastError string        `json:"last_error,omitempty"`
	SentAt    *time.Time    `json:"sent_at,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	// DeliveredAt is when the far end said the person's device has it;
	// OpenedAt, when they followed its link; Receipt, the far end's words.
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	OpenedAt    *time.Time `json:"opened_at,omitempty"`
	Receipt     string     `json:"receipt,omitempty"`
}

package notice

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DeclaredKind is one kind of event a producer says it sends, in words a
// person reads when they choose how to be told of it. Weight is the
// weight the producer usually sends it with; an event still carries its
// own. Producer is the token's name that declared it, set by dispatch.
type DeclaredKind struct {
	Producer    string `json:"producer,omitempty"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Weight      Weight `json:"weight"`
}

// MaxKinds is the most kinds one producer may declare.
const MaxKinds = 100

// ValidateKinds checks a producer's whole set the way the API takes it,
// and trims each title and description.
func ValidateKinds(kinds []DeclaredKind) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	if len(kinds) > MaxKinds {
		return bad("at most %d kinds", MaxKinds)
	}
	seen := map[string]bool{}
	for i, k := range kinds {
		k.Title, k.Description = strings.TrimSpace(k.Title), strings.TrimSpace(k.Description)
		switch {
		case !kindRe.MatchString(k.Kind) || len(k.Kind) > 200:
			return bad("kind %q must be dotted lowercase words, such as action.finished", k.Kind)
		case seen[k.Kind]:
			return bad("kind %s is listed twice", k.Kind)
		case k.Title == "" || utf8.RuneCountInString(k.Title) > 120:
			return bad("%s: title must be 1 to 120 characters", k.Kind)
		case utf8.RuneCountInString(k.Description) > 300:
			return bad("%s: description must be at most 300 characters", k.Kind)
		case k.Weight != WeightNotice && k.Weight != WeightNotify && k.Weight != WeightPage:
			return bad("%s: weight must be notice, notify, or page", k.Kind)
		}
		seen[k.Kind] = true
		kinds[i] = k
	}
	return nil
}

// Choice is how one person asked to be told of one kind of event: by
// these media, or by none, which keeps it on their list only.
type Choice struct {
	Kind  string
	Media []Medium
}

// ListOnly says whether the choice sends nothing.
func (c Choice) ListOnly() bool { return len(c.Media) == 0 }

// Choosing is the person with their rule for one weight replaced by the
// media they chose for a kind, all at once. No media is no message.
func (p Person) Choosing(w Weight, media []Medium) Person {
	rules := Rules{}
	for k, v := range p.Rules {
		rules[k] = v
	}
	steps := make([]RuleStep, 0, len(media))
	for _, m := range media {
		steps = append(steps, RuleStep{Medium: m})
	}
	rules[w] = steps
	p.Rules = rules
	return p
}

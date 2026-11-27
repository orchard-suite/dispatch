// Package signals is the watch's port to an estate's signal stores: a
// query, written in the store's own language with the subject's catalog
// fields as parameters, read over a window, answering a number or no
// data. Each store an estate keeps is an adapter (Prometheus, Azure
// Monitor's Log Analytics, and Azure's own platform metrics and Resource
// Health); Memory stands in for one in tests. Which query reads which
// signal for which blueprint is the estate's, in the routing document's
// settings (Settings), never in code. It has no storage.
package signals

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"
)

// Reader reads one query from one store.
type Reader interface {
	// Read answers the query's number over [From, To]. A query that
	// matched nothing answers ErrNoData.
	Read(ctx context.Context, q Query) (float64, error)
}

// Query is one query to read: its text with {name} parameters, their
// values, and the window.
type Query struct {
	Text   string
	Params map[string]string
	From   time.Time
	To     time.Time
}

// ErrNoData is a query that matched nothing: the store holds no signal for
// the subject, which is never the same as a clean one.
var ErrNoData = errors.New("no data")

// ErrMissingParam is a query that names a parameter the subject has no
// value for.
var ErrMissingParam = errors.New("the subject has no value for a parameter")

// paramName is the name of a parameter: a field or a part of one
// ({name}, {labels.app}), a relation's target ({namespace.title}), or what
// points at the subject ({<-k8s_namespace:workload.title}).
const paramName = `(?:<-[a-z0-9_]+:[a-z0-9_]+\.(?:key|title))|(?:[a-z][a-zA-Z0-9_./-]*)`

var paramNameRe = regexp.MustCompile(`^(?:` + paramName + `)$`)

// paramRe is a parameter in braces, or {window}. A brace a store's own
// language uses, such as PromQL's {code=~"5.."} or a regex's {1,5}, is
// not one.
var paramRe = regexp.MustCompile(`\{(` + paramName + `)\}`)

// Render fills a query's parameters, each written as the store's language
// quotes a value (escape), and {window} as the window in whole seconds,
// such as 1800s.
func Render(q Query, escape func(string) string) (string, error) {
	var missing []string
	out := paramRe.ReplaceAllStringFunc(q.Text, func(m string) string {
		name := m[1 : len(m)-1]
		if name == "window" {
			return fmt.Sprintf("%ds", int64(q.To.Sub(q.From).Seconds()))
		}
		v, ok := q.Params[name]
		if !ok || v == "" {
			missing = append(missing, name)
			return m
		}
		return escape(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%w: %s", ErrMissingParam, strings.Join(missing, ", "))
	}
	return out, nil
}

// number is a reading that is a number; NaN and infinity are no data.
func number(v float64) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, ErrNoData
	}
	return v, nil
}

// The kinds of store an adapter speaks to.
const (
	StorePrometheus    = "prometheus"
	StoreAzureMonitor  = "azure-monitor"
	StoreAzurePlatform = "azure-platform"
)

// Store is one signal store the estate keeps: its kind and where it is.
// A Prometheus store has a URL; an Azure Monitor one names the Log
// Analytics workspace, read as the pod's workload identity. An Azure
// platform store names nothing: it reads each resource's own metrics and
// Resource Health through Azure Resource Manager, as the pod's workload
// identity.
type Store struct {
	Kind      string `json:"kind" yaml:"kind"`
	URL       string `json:"url,omitempty" yaml:"url,omitempty"`
	Workspace string `json:"workspace,omitempty" yaml:"workspace,omitempty"`
}

// Signal is one query that reads one signal for things of one blueprint
// whose parameters equal every value in Where, and the effect kinds it
// shows: harm when its number is above Above or below Below. With
// neither set it is harm above 0.
type Signal struct {
	Blueprint string            `json:"blueprint" yaml:"blueprint"`
	Where     map[string]string `json:"where,omitempty" yaml:"where,omitempty"`
	Signal    string            `json:"signal" yaml:"signal"`
	Kinds     []string          `json:"kinds" yaml:"kinds"`
	Store     string            `json:"store" yaml:"store"`
	Query     string            `json:"query" yaml:"query"`
	Above     *float64          `json:"above,omitempty" yaml:"above,omitempty"`
	Below     *float64          `json:"below,omitempty" yaml:"below,omitempty"`
}

// Matches says whether a subject with these parameters is one the query
// reads: every value in Where equals the parameter of its name.
func (q Signal) Matches(params map[string]string) bool {
	for name, want := range q.Where {
		if got, ok := params[name]; !ok || got != want {
			return false
		}
	}
	return true
}

// Harm says whether a number is harm, and why.
func (q Signal) Harm(v float64) (bool, string) {
	if q.Above == nil && q.Below == nil {
		return v > 0, fmt.Sprintf("%g is above 0", v)
	}
	if q.Above != nil && v > *q.Above {
		return true, fmt.Sprintf("%g is above %g", v, *q.Above)
	}
	if q.Below != nil && v < *q.Below {
		return true, fmt.Sprintf("%g is below %g", v, *q.Below)
	}
	return false, ""
}

// Settings is the watch's part of the routing document: the stores and
// the queries.
type Settings struct {
	Stores  map[string]Store `json:"stores,omitempty" yaml:"stores,omitempty"`
	Queries []Signal         `json:"queries,omitempty" yaml:"queries,omitempty"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Validate checks the whole of the settings against the estate's kinds.
func (s Settings) Validate(kinds *change.Kinds) error {
	names := make([]string, 0, len(s.Stores))
	for name := range s.Stores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		st := s.Stores[name]
		if !slugRe.MatchString(name) {
			return fmt.Errorf("store %q: a store's name is a lowercase slug", name)
		}
		switch st.Kind {
		case StorePrometheus:
			if !strings.HasPrefix(st.URL, "http://") && !strings.HasPrefix(st.URL, "https://") {
				return fmt.Errorf("store %s: a prometheus store has an http or https url", name)
			}
		case StoreAzureMonitor:
			if !regexp.MustCompile(`^[0-9a-fA-F-]{36}$`).MatchString(st.Workspace) {
				return fmt.Errorf("store %s: an azure-monitor store names its Log Analytics workspace id", name)
			}
		case StoreAzurePlatform:
			if st.URL != "" || st.Workspace != "" {
				return fmt.Errorf("store %s: an azure-platform store names no url and no workspace; it reads each resource through Azure Resource Manager", name)
			}
		default:
			return fmt.Errorf("store %s: kind is %s, %s, or %s", name, StorePrometheus, StoreAzureMonitor, StoreAzurePlatform)
		}
	}
	for i, q := range s.Queries {
		at := fmt.Sprintf("query %d (%s %s)", i+1, q.Blueprint, q.Signal)
		switch {
		case !slugRe.MatchString(q.Blueprint):
			return fmt.Errorf("%s: blueprint is a catalog blueprint", at)
		case !slugRe.MatchString(q.Signal):
			return fmt.Errorf("%s: signal is a lowercase slug, such as restarts", at)
		case len(q.Kinds) == 0:
			return fmt.Errorf("%s: kinds names the effect kinds it shows", at)
		case strings.TrimSpace(q.Query) == "":
			return fmt.Errorf("%s: query is the store's query", at)
		case !finite(q.Above):
			return fmt.Errorf("%s: above is a number", at)
		case !finite(q.Below):
			return fmt.Errorf("%s: below is a number", at)
		case q.Above != nil && q.Below != nil && *q.Below > *q.Above:
			return fmt.Errorf("%s: below is at most above, or every number is harm", at)
		}
		wheres := make([]string, 0, len(q.Where))
		for name := range q.Where {
			wheres = append(wheres, name)
		}
		sort.Strings(wheres)
		for _, name := range wheres {
			if !paramNameRe.MatchString(name) || name == "window" {
				return fmt.Errorf("%s: where %q is not a parameter of a subject", at, name)
			}
			if q.Where[name] == "" {
				return fmt.Errorf("%s: where %s needs a value", at, name)
			}
		}
		st, ok := s.Stores[q.Store]
		if !ok {
			return fmt.Errorf("%s: there is no store %q", at, q.Store)
		}
		if st.Kind == StoreAzurePlatform {
			if _, err := ParsePlatform(q.Query); err != nil {
				return fmt.Errorf("%s: %v", at, err)
			}
		}
		for _, k := range q.Kinds {
			kind, ok := kinds.Get(k)
			if !ok {
				return fmt.Errorf("%s: %q is not an effect kind", at, k)
			}
			if kind.Family != change.FamilyDisruption || kind.Watch <= 0 {
				return fmt.Errorf("%s: %q is not a watched kind; only disruption kinds with a watch are read", at, k)
			}
		}
	}
	return nil
}

// finite is a threshold that is unset or a number.
func finite(v *float64) bool {
	return v == nil || (!math.IsNaN(*v) && !math.IsInf(*v, 0))
}

// For lists the queries that read a kind on a subject of a blueprint with
// these parameters.
func (s Settings) For(blueprint, kind string, params map[string]string) []Signal {
	var out []Signal
	for _, q := range s.Queries {
		if q.Blueprint == blueprint && slices.Contains(q.Kinds, kind) && q.Matches(params) {
			out = append(out, q)
		}
	}
	return out
}

// Look is what one query saw: harm, clean, or blind, and why when blind.
type Look struct {
	Signal  string
	Verdict string
	Why     string
}

// Judge is the verdict of a kind on a subject from what each of its
// queries saw: harm when any saw harm, else blind when there were none or
// any could not see, else clean. Blind is never clean.
func Judge(looks []Look) string {
	if len(looks) == 0 {
		return change.VerdictBlind
	}
	verdict := change.VerdictClean
	for _, l := range looks {
		switch l.Verdict {
		case change.VerdictHarm:
			return change.VerdictHarm
		case change.VerdictBlind:
			verdict = change.VerdictBlind
		}
	}
	return verdict
}

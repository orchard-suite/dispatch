package incident

import (
	"path"
	"strings"
)

// A Rule maps an alert's labels to a service. Rules are tried in order;
// the first whose every match fits wins. A match value is a glob
// (path.Match), so "kube-*" fits "kube-state-metrics".
type Rule struct {
	Match   map[string]string `json:"match"`
	Service string            `json:"service"`
}

// DefaultLabelOrder is where the service comes from when no rule fits:
// the first of these labels that is set.
var DefaultLabelOrder = []string{"service", "app", "namespace", "job"}

// ServiceFor picks the service for an alert's labels. With no rule and no
// label it is "unknown", so an incident always has a component to stand on.
func ServiceFor(labels map[string]string, rules []Rule) string {
	for _, r := range rules {
		if r.Service != "" && fits(labels, r.Match) {
			return r.Service
		}
	}
	for _, key := range DefaultLabelOrder {
		if v := strings.TrimSpace(labels[key]); v != "" {
			return v
		}
	}
	return "unknown"
}

func fits(labels, match map[string]string) bool {
	if len(match) == 0 {
		return false
	}
	for key, pattern := range match {
		ok, err := path.Match(pattern, labels[key])
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// ParseRules reads the settings form: one rule per line,
// "label=glob[,label=glob] -> service". Blank lines and # comments are
// skipped. A line it cannot read is returned so the form can say which.
func ParseRules(text string) ([]Rule, []string) {
	var rules []Rule
	var bad []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lhs, service, ok := strings.Cut(line, "->")
		service = strings.TrimSpace(service)
		if !ok || service == "" {
			bad = append(bad, line)
			continue
		}
		match := map[string]string{}
		for _, pair := range strings.Split(lhs, ",") {
			k, v, ok := strings.Cut(pair, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || k == "" || v == "" {
				match = nil
				break
			}
			match[k] = v
		}
		if len(match) == 0 {
			bad = append(bad, line)
			continue
		}
		rules = append(rules, Rule{Match: match, Service: service})
	}
	return rules, bad
}

// FormatRules is the inverse of ParseRules, for the settings form.
func FormatRules(rules []Rule) string {
	var b strings.Builder
	for _, r := range rules {
		keys := make([]string, 0, len(r.Match))
		for k := range r.Match {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k + "=" + r.Match[k])
		}
		b.WriteString(" -> " + r.Service + "\n")
	}
	return b.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Pages says whether an alert should page a person and so become an
// incident: a critical alert, or one labelled page=true or notify=sms.
// Every other alert is a notice to the channels that take it.
func Pages(labels map[string]string) bool {
	return ParseSeverity(labels["severity"]) == Critical || labels["page"] == "true" || labels["notify"] == "sms"
}

// Package changetest builds the kit's change events for tests: a change
// stated with the things it touches and its effects, and the events after
// it, each with the statement's digest.
package changetest

import (
	"encoding/json"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"
)

// Change is one change being written as events.
type Change struct {
	Sender    string
	Key       string
	Statement change.Statement
	Digest    string
}

// New states a change by sender, keyed key, touching things, with effects.
func New(sender, key string, things []change.Ref, effects ...change.Effect) Change {
	s := change.Statement{
		Key:      key,
		Writer:   change.Writer{Name: "infra", Version: "3.1.0"},
		Actor:    change.Actor{Kind: change.ActorAgent, ID: "deploy-bot"},
		Things:   things,
		Artifact: change.Artifact{Kind: "terraform-plan", Digest: "sha256:" + hex64},
		Applier:  change.ApplierHub,
		Effects:  effects,
	}
	d, err := s.Digest()
	if err != nil {
		panic(err)
	}
	return Change{Sender: sender, Key: key, Statement: s, Digest: d}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Stops is a stops effect on subject, in place.
func Stops(subject change.Ref) change.Effect {
	return change.Effect{Kind: "stops", Subject: subject, Path: change.PathInPlace, Undo: change.UndoYes, Claim: change.Claim{From: change.ClaimWriter}}
}

// Cuts is a cuts effect on subject, in place.
func Cuts(subject change.Ref) change.Effect {
	return change.Effect{Kind: "cuts", Subject: subject, Path: change.PathInPlace, Undo: change.UndoYes, Claim: change.Claim{From: change.ClaimWriter}}
}

// Grants is a grants effect on subject, in place.
func Grants(subject change.Ref) change.Effect {
	return change.Effect{Kind: "grants", Subject: subject, Path: change.PathInPlace, Undo: change.UndoYes, Claim: change.Claim{From: change.ClaimWriter}}
}

// Of names the change's stated event.
func (c Change) Of() *change.EventRef {
	return &change.EventRef{Sender: c.Sender, Key: c.Key + ".stated"}
}

func (c Change) head(event, suffix string, at time.Time) change.Header {
	h := change.Header{Event: event, Sender: c.Sender, Key: c.Key + suffix, At: at.UTC(), Digest: c.Digest}
	if event != change.EventStated {
		h.Of = c.Of()
	}
	return h
}

// Stated is the change.stated event.
func (c Change) Stated(at time.Time) []byte {
	return raw(&change.Stated{Header: c.head(change.EventStated, ".stated", at), Statement: c.Statement})
}

// Routed is the change.routed event, to no one or to one person.
func (c Change) Routed(to string, at time.Time) []byte {
	r := change.Route{To: to}
	if to == change.RoutePeople {
		r.Grants, r.Quorum = []change.Grant{{Values: []string{"approvers"}}}, 1
	}
	return raw(&change.Routed{Header: c.head(change.EventRouted, ".routed", at), Judge: &change.Judge{Name: "trellis", Version: "1.0.0", Pack: "gate"}, Route: r, Reasons: []change.RouteReason{{Rule: "a-rule"}}})
}

// Ended is the change.ended event with an outcome.
func (c Change) Ended(outcome string, at time.Time) []byte {
	return raw(&change.Ended{Header: c.head(change.EventEnded, ".ended", at), Outcome: outcome, Check: change.CheckMatched})
}

// Batch is a body for POST /v1/change-events.
func Batch(events ...[]byte) []byte {
	raws := make([]json.RawMessage, len(events))
	for i, e := range events {
		raws[i] = e
	}
	b, err := json.Marshal(map[string]any{"events": raws})
	if err != nil {
		panic(err)
	}
	return b
}

// Raws are events as a batch's list.
func Raws(events ...[]byte) []json.RawMessage {
	out := make([]json.RawMessage, len(events))
	for i, e := range events {
		out[i] = e
	}
	return out
}

// raw writes an event as JSON. It does not validate, so a test can send
// an estate's own kind or an event the desk must refuse.
func raw(e change.Event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return b
}

package signals

import (
	"context"
	"sync"
)

// Memory is a store held in memory, for tests: each rendered query
// answers the number or the error set for it, and a query set for nothing
// answers no data.
type Memory struct {
	mu      sync.Mutex
	answers map[string]answer
	// Asked is every rendered query read, in order.
	Asked []string
}

type answer struct {
	v   float64
	err error
}

// NewMemory makes an empty store.
func NewMemory() *Memory { return &Memory{answers: map[string]answer{}} }

// Set makes a rendered query answer v.
func (m *Memory) Set(query string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers[query] = answer{v: v}
}

// Fail makes a rendered query fail with err, as a store that is down.
func (m *Memory) Fail(query string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers[query] = answer{err: err}
}

// Read renders the query with its values as they are and answers what was
// set for it.
func (m *Memory) Read(_ context.Context, q Query) (float64, error) {
	text, err := Render(q, func(v string) string { return v })
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Asked = append(m.Asked, text)
	a, ok := m.answers[text]
	if !ok {
		return 0, ErrNoData
	}
	return a.v, a.err
}

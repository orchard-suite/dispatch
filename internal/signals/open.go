package signals

import (
	"errors"
	"fmt"
	"sync"
)

// Opener makes the adapter for each store the settings name, once, and
// keeps it while the store stays as it is.
type Opener struct {
	// tokens is the workload identity Azure Monitor stores read as; nil
	// when the pod has none, and every such store is then blind.
	tokens Tokens
	// fixed are adapters given by name, for tests, which win over the
	// settings.
	fixed map[string]Reader

	mu   sync.Mutex
	open map[string]opened
}

type opened struct {
	store  Store
	reader Reader
}

// NewOpener opens stores, reading Azure Monitor as tokens (nil for none).
func NewOpener(tokens Tokens) *Opener {
	return &Opener{tokens: tokens, fixed: map[string]Reader{}, open: map[string]opened{}}
}

// Use puts an adapter under a store's name, for a test.
func (o *Opener) Use(name string, r Reader) { o.fixed[name] = r }

// Open answers the adapter for a store.
func (o *Opener) Open(name string, st Store) (Reader, error) {
	if r, ok := o.fixed[name]; ok {
		return r, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if had, ok := o.open[name]; ok && had.store == st {
		return had.reader, nil
	}
	var r Reader
	switch st.Kind {
	case StorePrometheus:
		r = NewPrometheus(st.URL)
	case StoreAzureMonitor:
		if o.tokens == nil {
			return nil, errors.New("the desk has no workload identity to read Azure Monitor as")
		}
		r = NewAzureMonitor(st.Workspace, o.tokens)
	default:
		return nil, fmt.Errorf("no adapter for a %q store", st.Kind)
	}
	o.open[name] = opened{store: st, reader: r}
	return r, nil
}

package signals

import (
	"errors"
	"fmt"
	"sync"
)

// SignIn gives the pod's workload identity's tokens for one scope.
type SignIn func(scope string) (Tokens, error)

// Opener makes the adapter for each store the settings name, once, and
// keeps it while the store stays as it is.
type Opener struct {
	// signIn is the workload identity Azure stores read as; nil when the
	// pod has none, and every such store is then blind.
	signIn SignIn
	// tokens is the identity signed in for each scope, made once.
	tokens map[string]Tokens
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

// NewOpener opens stores, reading Azure as signIn (nil for none).
func NewOpener(signIn SignIn) *Opener {
	return &Opener{signIn: signIn, tokens: map[string]Tokens{}, fixed: map[string]Reader{}, open: map[string]opened{}}
}

// tokensFor is the workload identity for a scope. The caller holds mu.
func (o *Opener) tokensFor(scope string) (Tokens, error) {
	if o.signIn == nil {
		return nil, errors.New("the desk has no workload identity to read Azure as")
	}
	if t, ok := o.tokens[scope]; ok {
		return t, nil
	}
	t, err := o.signIn(scope)
	if err != nil {
		return nil, err
	}
	o.tokens[scope] = t
	return t, nil
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
		t, err := o.tokensFor(AzureMonitorScope)
		if err != nil {
			return nil, err
		}
		r = NewAzureMonitor(st.Workspace, t)
	case StoreAzurePlatform:
		t, err := o.tokensFor(AzurePlatformScope)
		if err != nil {
			return nil, err
		}
		r = NewAzurePlatform(t)
	default:
		return nil, fmt.Errorf("no adapter for a %q store", st.Kind)
	}
	o.open[name] = opened{store: st, reader: r}
	return r, nil
}

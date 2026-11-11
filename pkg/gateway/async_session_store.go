package gateway

// Async boundary for the synchronous thread safe SessionStore, each call running on the caller goroutine
type AsyncSessionStore struct {
	store *SessionStore
}

// Facade over store
func NewAsyncSessionStore(store *SessionStore) *AsyncSessionStore {
	return &AsyncSessionStore{store: store}
}

// Wrapped store whose methods the facade offloads
func (a *AsyncSessionStore) Store() *SessionStore { return a.store }

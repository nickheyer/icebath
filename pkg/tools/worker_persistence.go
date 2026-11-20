package tools

// Python WorkerPersistenceError raised when a worker process cannot persist through its owner
type WorkerPersistenceError struct{ Reason string }

// Builds the error a worker raises with the owner rpc reason code
func NewWorkerPersistenceError(reason string) error { return &WorkerPersistenceError{Reason: reason} }

func (e *WorkerPersistenceError) Error() string { return e.Reason }

func (e *WorkerPersistenceError) PyTypeName() string { return "WorkerPersistenceError" }

// Marks the env a non dispatcher owned execution and returns the restore of the previous flag
func EnterNonDispatcherOwnedContext(env *Env) func() {
	if env == nil || env.Values == nil {
		panic("EnterNonDispatcherOwnedContext needs an env with a Values map")
	}
	prev, had := env.Values.Load("session.non_dispatcher_owned")
	env.Values.Store("session.non_dispatcher_owned", true)
	return func() {
		if had {
			env.Values.Store("session.non_dispatcher_owned", prev)
			return
		}
		env.Values.Delete("session.non_dispatcher_owned")
	}
}

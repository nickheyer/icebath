package tools

import "sync"

// Context copy a spawned task inherits, sharing no session variable with the parent env afterwards
func CopySessionContext(src *Env) *Env {
	dst := *src
	dst.Values = &sync.Map{}
	if src.Values != nil {
		src.Values.Range(func(k, v any) bool {
			dst.Values.Store(k, v)
			return true
		})
	}
	return &dst
}

// Answer the clarify settled with, nil while unanswered or after a timeout
func (e *ClarifyEntry) Response() *string {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	return e.response
}

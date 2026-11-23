package tuiserver

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

const kernelSessionKey = "20260907_120000_abcdef"

// Session record of the kernel cases
func kernelSession() *tuihost.Session {
	return tuihost.NewSession(map[string]any{"active_session_lease": nil, "agent": nil, "history": []any{}, "history_lock": &sync.Mutex{},
		"profile_home": nil, "session_key": kernelSessionKey, "source": "tui"})
}

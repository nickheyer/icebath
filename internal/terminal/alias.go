package terminal

import (
	"github.com/nickheyer/icebath/internal/config"
)

// Has the session's commands run in the environments of the parent session, as a subagent works in the sandbox of the agent that delegated
// to it, starting in the folder given; its own folder kept apart from the parent's from then on
func (t *Terminal) Alias(cfg *config.Config, session, parent, cwd string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if session == "" || parent == "" || session == parent {
		return
	}
	t.aliases[sessionKey(cfg, session)] = parent
	if cwd != "" {
		t.folders[sessionKey(cfg, session)] = cwd
	}
}

// Session whose environments the session's commands run in: itself, or the session it is an alias of, followed to its end
func (t *Terminal) aliasOf(cfg *config.Config, session string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	for !seen[session] {
		seen[session] = true
		parent, ok := t.aliases[sessionKey(cfg, session)]
		if !ok {
			break
		}
		session = parent
	}
	return session
}

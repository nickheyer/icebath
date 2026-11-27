package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

// Snapshots the projects the agent's calls are about to change, at most once per project for each reply of the model, and notes what
// the calls wrote, so /rollback puts the agent's changes back and keeps the user's
type registrant struct {
	tools    *tools.Registry
	terminal *terminal.Terminal
	mu       sync.Mutex
	// Snapshot each project of a session was taken as of in the reply running now, by session and folder
	taken map[string]map[string]string
	// Project and snapshot of each running shell command that may change files, by call
	commands map[string]command
}

type command struct {
	dir, sha string
}

// Registers the snapshots before tool calls; what forgets a session, once it is done
func Register(registry *tools.Registry, host *terminal.Terminal) func(session string) {
	r := &registrant{tools: registry, terminal: host, taken: map[string]map[string]string{}, commands: map[string]command{}}
	registry.Register(r)
	return r.forget
}

func (r *registrant) Tools() []tools.Def { return nil }

// A new reply of the model: each project is snapshotted again before its calls change it
func (r *registrant) Stepped(session string) {
	r.forget(session)
}

func (r *registrant) forget(session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.taken, session)
}

// Commands that change or delete files: removals, copies and moves over files, in place edits, truncations, raw writes, and git
// operations that rewrite the worktree
var destructive = regexp.MustCompile("(?:^|\\s|&&|\\|\\||;|`)(?:rm\\s|rmdir\\s|cp\\s|install\\s|mv\\s|sed\\s+-i|truncate\\s|dd\\s|shred\\s|git\\s+(?:reset|clean|checkout)\\s)")

// A redirect that overwrites a file
var overwrite = regexp.MustCompile(`[^>]>[^>]|^>[^>]`)

// Whether a shell command looks like it changes or deletes files
func changes(command string) bool {
	return command != "" && (destructive.MatchString(command) || overwrite.MatchString(command))
}

// Store of the call's profile when snapshots are on and serve its terminal backend, nil otherwise
func (r *registrant) store(call *tools.Call) *checkpoints.Store {
	store := checkpoints.Open(call.Env.Config)
	if !store.Enabled() {
		return nil
	}
	backend, err := r.terminal.Configured(call.Env.Config)
	if err != nil || checkpoints.Refusal(backend) != "" {
		return nil
	}
	return store
}

// Folder a path of the call names, read from where the session's commands run: the folder they last ended in, else the session's
func (r *registrant) resolve(call *tools.Call, path string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	base := r.terminal.Folder(call.Env.Config, call.SessionID)
	if base == "" {
		base = call.Env.Cwd
	}
	return filepath.Join(base, path)
}

// Whether a path is one of the Windows namespaces, \\?\ or \\.\, that snapshots of folders can't take
func namespaced(path string) bool {
	return strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`)
}

// Snapshots the project unless the session's running reply already did: the snapshot its files are as of
func (r *registrant) take(session string, store *checkpoints.Store, dir, reason string) string {
	r.mu.Lock()
	if sha, done := r.taken[session][dir]; done {
		r.mu.Unlock()
		return sha
	}
	if r.taken[session] == nil {
		r.taken[session] = map[string]string{}
	}
	r.taken[session][dir] = ""
	r.mu.Unlock()
	sha := store.Take(dir, reason)
	r.mu.Lock()
	if r.taken[session] != nil {
		r.taken[session][dir] = sha
	}
	r.mu.Unlock()
	return sha
}

// A call is about to run: the projects of the files it writes are snapshotted, and the folder of a shell command that changes files
func (r *registrant) Dispatched(call *tools.Call) {
	store := r.store(call)
	if store == nil {
		return
	}
	if call.Name == "terminal" {
		text, _ := call.Args["command"].(string)
		if !changes(text) {
			return
		}
		dir := ""
		if workdir, _ := call.Args["workdir"].(string); strings.TrimSpace(workdir) != "" {
			dir = r.resolve(call, workdir)
		} else {
			dir = r.resolve(call, ".")
		}
		project := store.Project(dir)
		reason := "before terminal: " + string([]rune(text)[:min(60, len([]rune(text)))])
		if sha := r.take(call.SessionID, store, project, reason); sha != "" {
			r.mu.Lock()
			r.commands[call.ToolCallID] = command{project, sha}
			r.mu.Unlock()
		}
		return
	}
	def, ok := r.tools.Def(call.Name)
	if !ok || def.Paths == nil {
		return
	}
	paths, writes := def.Paths(call.Env, call.Args)
	if !writes {
		return
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" || namespaced(path) {
			continue
		}
		r.take(call.SessionID, store, store.Project(r.resolve(call, path)), "before "+call.Name)
	}
}

// A call returned: what a file write landed is noted as the agent's, and so is every file a shell command changed
func (r *registrant) Finished(call *tools.Call, result string) {
	r.mu.Lock()
	ran, shell := r.commands[call.ToolCallID]
	delete(r.commands, call.ToolCallID)
	r.mu.Unlock()
	store := r.store(call)
	if store == nil {
		return
	}
	if shell {
		store.RecordSince(ran.dir, ran.sha)
		return
	}
	def, ok := r.tools.Def(call.Name)
	if !ok {
		return
	}
	for _, path := range tools.Landed(def, call, result) {
		if !namespaced(path) {
			store.Record(r.resolve(call, path))
		}
	}
}

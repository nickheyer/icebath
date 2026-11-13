package logging

import (
	"context"
	"path"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

// Named file the readers find below the logs directory of the profile home or of the install root
type Log struct {
	Name  string
	File  string
	Root  bool
	entry home.Entry
	root  home.RootEntry
}

// Known log files in the order the readers list them
var logs = [...]Log{
	{Name: "agent", entry: home.AgentLog},
	{Name: "errors", entry: home.ErrorsLog},
	{Name: "gateway", entry: home.GatewayLog},
	{Name: "gui", entry: home.GUILog},
	{Name: "desktop", entry: home.DesktopLog},
	{Name: "update", Root: true, root: home.UpdateLog},
	{Name: "handoff", Root: true, root: home.DesktopUpdateHandoffLog},
	{Name: "mcp", entry: home.MCPStderrLog},
}

// Known log files sorted by name with their file names filled
func Logs() []Log {
	out := make([]Log, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.filled())
	}
	slices.SortFunc(out, func(a, b Log) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Known log file of a name, false for a name the readers do not know
func LookupLog(name string) (Log, bool) {
	for _, l := range logs {
		if l.Name == name {
			return l.filled(), true
		}
	}
	return Log{}, false
}

// Log with its file name taken from its location
func (l Log) filled() Log {
	if l.Root {
		l.File = path.Base(string(l.root))
	} else {
		l.File = path.Base(string(l.entry))
	}
	return l
}

// Location of the file for ctx, below the install root or the home of ctx, existence unchecked
func (l Log) Path(ctx context.Context, r *home.Resolver) (string, error) {
	if l.Root {
		root, err := home.DefaultRoot()
		if err != nil {
			return "", err
		}
		return l.root.In(root), nil
	}
	h, err := r.Home(ctx)
	if err != nil {
		return "", err
	}
	return l.entry.In(h), nil
}

// Source name prefixes of each component, gating the component log files and the component line filter
var components = [...]struct {
	name     string
	prefixes []string
}{
	{"agent", []string{"agent", "memory", "model", "monitor", "research.batch"}},
	{"cli", []string{"cli"}},
	{"cron", []string{"cron"}},
	{"gateway", []string{"gateway", "plugins", "memory.callerscope"}},
	{"gui", []string{"dashboard", "desktop", "plugin.desktop", "rpc"}},
	{"tools", []string{"tool"}},
}

// Component names sorted, as the readers offer them
func Components() []string {
	out := make([]string, 0, len(components))
	for _, c := range components {
		out = append(out, c.name)
	}
	return out
}

// Source name prefixes of a component, false for an unknown component
func ComponentPrefixes(name string) ([]string, bool) {
	for _, c := range components {
		if c.name == name {
			return slices.Clone(c.prefixes), true
		}
	}
	return nil, false
}

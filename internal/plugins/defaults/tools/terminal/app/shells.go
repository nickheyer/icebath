package app

import (
	"bufio"
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Id of the choice that types a shell the list lacks
const typeShell = "\x00"

// Shells /etc/shells lists that no one types in: those that only refuse a sign-in, the restricted ones, and those that serve one program
var unusable = []string{"nologin", "false", "true", "rbash", "rzsh", "rksh", "rssh", "scponly"}

// The shells this computer lists in /etc/shells that are there and that a person types in, each once
func shells() []string {
	f, err := os.Open("/etc/shells")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name := filepath.Base(line)
		if line == "" || strings.HasPrefix(line, "#") || !fsx.IsFile(line) || slices.Contains(unusable, name) ||
			strings.HasSuffix(name, "-shell") {
			continue
		}
		// One shell listed in two folders that are one, such as /bin and /usr/bin, is listed once; sh is kept apart from the shell it
		// may be a link to, as the name a person knows it by
		folder, err := filepath.EvalSymlinks(filepath.Dir(line))
		if err != nil {
			folder = filepath.Dir(line)
		}
		if at := filepath.Join(folder, name); !seen[at] {
			seen[at] = true
			out = append(out, line)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return strings.Compare(filepath.Base(a), filepath.Base(b)) })
	return out
}

// Picker of the shell a terminal window runs: your usual one, a shell this computer lists, or one typed
func pickShell(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
	key := cmp.Or(ask.Key, terminalconfig.Shell)
	var current string
	yaml.Unmarshal([]byte(ask.Values[key]), &current)
	first := "Your usual shell"
	if len(ask.First) > 0 {
		first = ask.First[0].Title
	}
	keep := func(value string) tea.Cmd {
		if value == "" {
			return ask.Done(map[string]string{key: "null"})
		}
		return ask.Done(map[string]string{key: strconv.Quote(value)})
	}
	choices := []tui.Choice{{ID: "", Title: first, Detail: "The shell your account logs in with"}}
	for _, path := range shells() {
		choices = append(choices, tui.Choice{ID: path, Title: filepath.Base(path), Detail: path})
	}
	choices = append(choices, tui.Choice{ID: typeShell, Title: "Type the command", Detail: "A shell this list lacks, or one with options"})
	return kit.PickFrom(hub, cmp.Or(ask.Title, "Shell"), ask.Intro, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
		if choice.ID != typeShell {
			return keep(choice.ID)
		}
		field := cards.Field{Label: "Shell", Value: current, Path: cards.PathProgram,
			Help: "The command that starts the shell, with what it is handed, such as /usr/bin/fish --login."}
		return kit.Ask(hub, "Shell", field, "Save", func(text string) tea.Cmd { return keep(strings.TrimSpace(text)) })
	})
}

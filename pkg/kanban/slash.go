package kanban

import (
	"bytes"
	"context"
	"fmt"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// What the CLI parser made of slash tokens, parser text renamed to /kanban whenever no action parsed
type SlashParse struct {
	Namespace map[string]any
	Parsed    bool
	Out, Err  string
	ArgError  string
	Usage     string
	Failure   error
}

// Parses slash tokens with the kanban CLI parser shown as /kanban, assigned in cmd/icebath wiring
var ParseSlash func(tokens []string) SlashParse

// Curated short help a bare or help slash invocation shows instead of the full parser usage
const SlashKanbanHelp = "**/kanban** — manage the shared task board.\n" +
	"\n" +
	"Common subcommands:\n" +
	"  `list` (alias `ls`)   List tasks on the current board\n" +
	"  `show <id>`           Task details + comments + events\n" +
	"  `stats`               Per-status / per-assignee counts\n" +
	"  `create <title>…`     Create a task (auto-subscribes you to events)\n" +
	"  `comment <id> <msg>`  Append a comment\n" +
	"  `attach <id> <path>`  Attach a local file; `attachments <id>` to list\n" +
	"  `complete <id>…`      Mark task(s) done\n" +
	"  `request-review <id>` Enter first-class review; `request-changes <id> <reason>` returns an active review to its implementer\n" +
	"  `block <id> [reason]` Mark blocked; `schedule <id> [reason]` parks time-delay work; `unblock <id>` to revive\n" +
	"  `assign <id> <profile>`  Reassign\n" +
	"  `boards list`         Show all boards\n" +
	"  `assignees`           Known profiles + counts\n" +
	"  `context <id>`        Full worker-context dump\n" +
	"  `runs <id>`           Attempt history\n" +
	"  `log <id>`            Worker log\n" +
	"\n" +
	"Run `/kanban <subcommand> -h` for arguments. " +
	"Read-only commands are safe while an agent is running."

// Slash tokens of a kanban invocation, non POSIX on Windows with one wrapping quote pair stripped
func slashTokens(rest string) ([]string, error) {
	if rest == "" || pystr.Strip(rest) == "" {
		return nil, nil
	}
	var raw []string
	var err error
	if runtime.GOOS == "windows" {
		raw, err = pyshlex.SplitNonPOSIX(rest)
	} else {
		raw, err = pyshlex.SplitPOSIX(rest, false)
	}
	if err != nil {
		return nil, err
	}
	tokens := make([]string, 0, len(raw))
	for _, tok := range raw {
		if len(tok) >= 2 && tok[0] == tok[len(tok)-1] && (tok[0] == '\'' || tok[0] == '"') {
			tok = tok[1 : len(tok)-1]
		}
		tokens = append(tokens, tok)
	}
	return tokens, nil
}

// Runs the text after /kanban through the kanban CLI and returns its captured output, shared by the CLI and the gateway
func RunSlash(ctx context.Context, rest string) (string, error) {
	tokens, err := slashTokens(rest)
	if err != nil {
		return "", err
	}
	if len(tokens) == 0 || tokens[0] == "help" || tokens[0] == "--help" || tokens[0] == "-h" || tokens[0] == "?" {
		return SlashKanbanHelp, nil
	}
	p := ParseSlash(tokens)
	if !p.Parsed {
		out := pystr.Rstrip(p.Out)
		if p.ArgError != "" {
			return "⚠ /kanban usage error\n" + p.Usage + "\n" + p.ArgError, nil
		}
		if p.Failure == nil && out != "" {
			return out, nil
		}
		body := pystr.Rstrip(p.Err)
		if body == "" {
			body = out
		}
		if body == "" && p.Failure != nil {
			body = p.Failure.Error()
		}
		if body != "" {
			return "⚠ /kanban usage error\n" + body, nil
		}
		return "⚠ /kanban usage error", nil
	}
	var bufOut, bufErr bytes.Buffer
	bufOut.WriteString(p.Out)
	bufErr.WriteString(p.Err)
	if _, kerr := KanbanCommand(ctx, p.Namespace, &bufOut, &bufErr); kerr != nil {
		fmt.Fprintf(&bufErr, "error: %s\n", pyrepr.ExceptionMessage(kerr))
	}
	out, errText := pystr.Rstrip(bufOut.String()), pystr.Rstrip(bufErr.String())
	if errText != "" && out != "" {
		return out + "\n" + errText, nil
	}
	if errText != "" {
		return errText, nil
	}
	if out == "" {
		return "(no output)", nil
	}
	return out, nil
}

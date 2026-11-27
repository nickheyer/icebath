package tools

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/terminal"
	hostconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
)

// Longest the user is waited on for a sudo password
const sudoWait = 45 * time.Second

// The command with each sudo it runs reading its password from stdin, and that stdin: the configured password, else the one the session
// gave, else one the user is asked for when sudo can't run without one; the command as it was when no password is had
func (r *registrant) sudo(ctx context.Context, call *tools.Call, lease *terminal.Lease, command string) (string, *string) {
	rewritten, count := terminal.RewriteSudo(command)
	if count == 0 {
		return command, nil
	}
	cfg := call.Env.Config
	password := cfg.GetString(hostconfig.SudoPassword)
	if password == "" {
		password = r.host.SudoPassword(cfg, call.SessionID)
	}
	if password == "" && call.Env.Clarify != nil {
		if sudoer, ok := lease.Env.(terminal.Sudoer); ok && sudoer.SudoWithoutPassword(ctx) {
			return command, nil
		}
		if password = askSudo(ctx, call.Env, command); password != "" {
			r.host.SetSudoPassword(cfg, call.SessionID, password)
		}
	}
	if password == "" {
		return command, nil
	}
	stdin := strings.Repeat(password+"\n", count)
	return rewritten, &stdin
}

// Asks the user for the sudo password, empty when they skip it, time runs out or nobody answers
func askSudo(ctx context.Context, env *tools.Env, command string) string {
	ctx, cancel := context.WithTimeout(ctx, sudoWait)
	defer cancel()
	reply, err := env.Clarify(ctx, []tools.Question{{
		ID:     "sudo",
		Secret: true,
		Text: "Sudo password required. The command `" + command + "` needs sudo. Enter the password, or submit an empty answer to skip (the " +
			"command fails gracefully). It is kept for this session only; set it in /settings under Terminal to be asked no more.",
	}})
	if err != nil || reply.Outcome != tools.Submitted || len(reply.Answers["sudo"]) == 0 {
		return ""
	}
	return reply.Answers["sudo"][0]
}

// Output with what to do about a failed sudo, and whether sudo turned the password down and the session's one was forgotten for it
func (r *registrant) sudoNotes(call *tools.Call, command, output string) (string, bool, bool) {
	cfg := call.Env.Config
	configured := cfg.GetString(hostconfig.SudoPassword) != ""
	if call.Env.Clarify == nil && terminal.SudoHeadless(output) {
		output += "\n\n💡 Tip: This session cannot prompt for a sudo password. Set the sudo password in /settings under Terminal (or SUDO_PASSWORD " +
			"in the environment icebath runs in), or run the command without sudo."
	}
	failed := terminal.SudoWrongPassword(output)
	_, sudos := terminal.RewriteSudo(command)
	cleared := false
	if !configured && failed && sudos > 0 && r.host.SudoPassword(cfg, call.SessionID) != "" {
		r.host.SetSudoPassword(cfg, call.SessionID, "")
		cleared = true
	}
	if cleared && call.Env.Clarify != nil {
		output += "\n\n⚠️ Sudo authentication failed — cached password cleared. You will be prompted again on the next sudo command."
	}
	return output, failed, cleared
}

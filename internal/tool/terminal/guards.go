package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const workdirSafeASCII = "/\\:_-.~ +@=,"

func safeWorkdirChar(r rune) bool {
	if r < 32 || r == 127 {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune(workdirSafeASCII, r)
}

func charRepr(r rune) string {
	if r == '\'' {
		return `"'"`
	}
	var b strings.Builder
	b.WriteByte('\'')
	switch {
	case r == '\\':
		b.WriteString(`\\`)
	case r == '\n':
		b.WriteString(`\n`)
	case r == '\r':
		b.WriteString(`\r`)
	case r == '\t':
		b.WriteString(`\t`)
	case unicode.IsPrint(r):
		b.WriteRune(r)
	case r < 0x100:
		fmt.Fprintf(&b, `\x%02x`, r)
	case r < 0x10000:
		fmt.Fprintf(&b, `\u%04x`, r)
	default:
		fmt.Fprintf(&b, `\U%08x`, r)
	}
	b.WriteByte('\'')
	return b.String()
}

func validateWorkdir(workdir string) (string, bool) {
	for _, r := range workdir {
		if !safeWorkdirChar(r) {
			return fmt.Sprintf("Blocked: workdir contains disallowed character %s. "+
				"Use a simple filesystem path without shell metacharacters.", charRepr(r)), true
		}
	}
	return "", false
}

var (
	shellLevelBackgroundRe = regexp.MustCompile(`(?im)(?:^|[;&|]\s*|&&\s*|\|\|\s*|\$\(\s*)(?:nohup|disown|setsid)\b`)
	inlineBackgroundAmpRe  = regexp.MustCompile(`\s&\s`)
	trailingBackgroundRe   = regexp.MustCompile(`\s&\s*(?:#.*)?\n?\z`)
	singleQuotedRe         = regexp.MustCompile(`'[^']*'`)
	doubleQuotedRe         = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	backtickRe             = regexp.MustCompile("`[^`]*`")
	nonBackgroundAmpRe     = regexp.MustCompile(`&&|\d*[<>]&(?:\d+|-)?|&>>?|\|&`)
	trailingAmpOnlyRe      = regexp.MustCompile(`\s&\s*\z`)
	longLivedForeground    = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?(?:dev|start|serve|watch)\b`),
		regexp.MustCompile(`(?i)\bdocker\s+compose\s+up\b`),
		regexp.MustCompile(`(?i)\bnext\s+dev\b`),
		regexp.MustCompile(`(?i)\bvite(?:\s|\z)`),
		regexp.MustCompile(`(?i)\bnodemon\b`),
		regexp.MustCompile(`(?i)\buvicorn\b`),
		regexp.MustCompile(`(?i)\bgunicorn\b`),
		regexp.MustCompile(`(?i)\bpython(?:3)?\s+-m\s+http\.server\b`),
	}
)

const shellWrapperGuidance = "Foreground command uses shell-level background wrappers (nohup/disown/setsid). " +
	"Re-send WITHOUT the wrapper as terminal(command=\"<cmd>\", background=true, " +
	"notify_on_complete=true) so icebath tracks the process, then run readiness " +
	"checks and tests in separate commands."

const ampGuidance = "Foreground command uses '&' backgrounding. Re-send WITHOUT the '&' as " +
	"terminal(command=\"<cmd>\", background=true) — add notify_on_complete=true " +
	"for bounded jobs — then run health checks and tests in follow-up terminal calls."

const longLivedGuidance = "This foreground command appears to start a long-lived server/watch process. " +
	"Run it with background=true, verify readiness (health endpoint/log signal), " +
	"then execute tests in a separate command."

func stripQuotes(command string) string {
	result := approval.StripInertHeredocs(command)
	result = singleQuotedRe.ReplaceAllLiteralString(result, "''")
	result = doubleQuotedRe.ReplaceAllLiteralString(result, `""`)
	return backtickRe.ReplaceAllLiteralString(result, "``")
}

func looksLikeHelpOrVersion(command string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(command)), " ")
	return strings.Contains(normalized, " --help") || strings.HasSuffix(normalized, " -h") ||
		strings.Contains(normalized, " --version") || strings.HasSuffix(normalized, " -v")
}

func foregroundGuidance(command string) (string, bool) {
	if looksLikeHelpOrVersion(command) {
		return "", false
	}
	unquoted := stripQuotes(command)
	if shellLevelBackgroundRe.MatchString(unquoted) {
		return shellWrapperGuidance, true
	}
	if inlineBackgroundAmpRe.MatchString(unquoted) || trailingBackgroundRe.MatchString(unquoted) {
		return ampGuidance, true
	}
	for _, p := range longLivedForeground {
		if p.MatchString(unquoted) {
			return longLivedGuidance, true
		}
	}
	return "", false
}

func stripTrailingAmp(command string) string {
	body := []rune(strings.TrimRightFunc(command, unicode.IsSpace))
	body = body[:len(body)-1]
	end := len(body)
	for end > 0 && unicode.IsSpace(body[end-1]) {
		prefix := body[:end-1]
		trimmed := strings.TrimRight(string(prefix), "\\")
		if (utf8.RuneCountInString(string(prefix))-utf8.RuneCountInString(trimmed))%2 == 1 {
			break
		}
		end--
	}
	return string(body[:end])
}

type approvalIdentity struct {
	hardline     bool
	hardlineDesc string
	dangerous    bool
	dangerKey    string
	dangerDesc   string
	sudo         bool
	sudoDesc     string
	runtime      string
	runtimeHit   bool
	deny         string
	denyHit      bool
	allowlisted  bool
}

func (t *Terminal) approvalIdentity(ctx context.Context, c *tool.Call, command string) (approvalIdentity, error) {
	det, err := t.o.Gate.Detector(ctx, c)
	if err != nil {
		return approvalIdentity{}, err
	}
	var denyGlobs []string
	if cfg, err := t.o.Config.Load(ctx); err == nil {
		denyGlobs = cfg.Approvals.Deny
	} else {
		slog.Debug("approval deny rules read failed", "err", err)
	}
	sudoConfigured := false
	if c.Secrets != nil {
		_, ok, err := c.Secrets.Lookup(ctx, "SUDO_PASSWORD")
		sudoConfigured = err == nil && ok
	}
	var id approvalIdentity
	id.hardline, id.hardlineDesc = det.Hardline(command)
	id.dangerous, id.dangerKey, id.dangerDesc = det.Dangerous(command)
	id.sudo, id.sudoDesc = det.SudoStdin(command, sudoConfigured)
	if p := t.o.Gate.Protector(); p != nil {
		id.runtime, id.runtimeHit = p.Command(command)
	}
	id.deny, id.denyHit = det.DenyRule(command, denyGlobs)
	id.allowlisted = approval.AllowlistMatch(command, t.o.Gate.Permanent(ctx))
	return id, nil
}

func (t *Terminal) trailingAmpBackground(ctx context.Context, c *tool.Call, command string) (*string, error) {
	if looksLikeHelpOrVersion(command) {
		return nil, nil
	}
	for _, marker := range []string{"$(", "`", "<(", ">("} {
		if strings.Contains(command, marker) {
			return nil, nil
		}
	}
	unquoted := stripQuotes(command)
	if !trailingAmpOnlyRe.MatchString(unquoted) || !strings.HasSuffix(strings.TrimRightFunc(command, unicode.IsSpace), "&") {
		return nil, nil
	}
	body := strings.TrimRightFunc(unquoted, unicode.IsSpace)
	body = body[:len(body)-1]
	if strings.ContainsAny(body, "\n\r;#") || shellLevelBackgroundRe.MatchString(body) {
		return nil, nil
	}
	if strings.Contains(nonBackgroundAmpRe.ReplaceAllLiteralString(body, ""), "&") {
		return nil, nil
	}
	stripped := stripTrailingAmp(command)
	if strings.TrimSpace(stripped) == "" {
		return nil, nil
	}
	before, err := t.approvalIdentity(ctx, c, command)
	if err != nil {
		return nil, err
	}
	after, err := t.approvalIdentity(ctx, c, stripped)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, nil
	}
	return &stripped, nil
}

type guardOutcome struct {
	blocked *tool.Result
	err     error
}

func (t *Terminal) boundedPreExec(ctx context.Context, c *tool.Call, timeout time.Duration, command string, e tool.Environment, kind, cwd, workdir, sessionKey string) (*tool.Result, error) {
	gctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ch := make(chan guardOutcome, 1)
	go func() {
		r, err := t.preExec(gctx, c, command, e, kind, cwd, workdir, sessionKey)
		ch <- guardOutcome{r, err}
	}()
	timedOut := func() (*tool.Result, error) {
		return nil, &rejected{errorJSON(fmt.Sprintf("Terminal pre-execution guard did not finish within %ss "+
			"(process-identity probe wedged); the command was not run. Retry the call.", formatNumber(timeout.Seconds())), -1, "error")}
	}
	select {
	case o := <-ch:
		if o.err != nil && ctx.Err() == nil && errors.Is(gctx.Err(), context.DeadlineExceeded) {
			return timedOut()
		}
		return o.blocked, o.err
	case <-gctx.Done():
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return timedOut()
	}
}

func (t *Terminal) preExec(ctx context.Context, c *tool.Call, command string, e tool.Environment, kind, cwd, workdir, sessionKey string) (*tool.Result, error) {
	if r, err := t.gatewayLifecycleBlock(ctx, c, command, e, kind, cwd, workdir, sessionKey); err != nil || r != nil {
		return r, err
	}
	if workdir != "" {
		if msg, bad := validateWorkdir(workdir); bad {
			slog.Warn(fmt.Sprintf("Blocked dangerous workdir: %s (command: %s)", firstRunes(workdir, 200), commandPreview(command)))
			r := errorJSON(msg, -1, "blocked")
			return &r, nil
		}
	}
	if kind != "local" {
		return nil, nil
	}
	if r, err := t.selfRepoBlock(ctx, command, cwd, workdir, sessionKey); err != nil || r != nil {
		return r, err
	}
	return t.lockedSkillBlock(ctx, command, cwd, workdir, sessionKey)
}

func (t *Terminal) gatewayLifecycleBlock(ctx context.Context, c *tool.Call, command string, e tool.Environment, kind, cwd, workdir, sessionKey string) (*tool.Result, error) {
	if !t.o.Role.SupervisedGateway() {
		return nil, nil
	}
	det, err := t.o.Gate.Detector(ctx, c)
	if err != nil {
		return nil, err
	}
	if lifecycleScanRootWithinBudget(command) && containsLaunchctlSubmit(command) {
		r := blockedJSON("Blocked: launchctl submit/bootstrap is restricted inside a supervised "+
			"gateway regardless of the job label, to prevent indirect gateway "+
			"restart loops. This guard does not inspect the job's KeepAlive settings "+
			"or determine whether it is independent of icebath. Perform authorized "+
			"LaunchAgent maintenance from a separate shell outside the gateway, "+
			"not by switching launchctl verbs to bypass this rejection.", "error")
		return &r, nil
	}
	base, recorded := t.o.Envs.SessionCwd(ctx, sessionKey)
	if !recorded {
		base = cwd
		if e != nil && e.Cwd() != "" {
			base = e.Cwd()
		}
	}
	mountedHost, _, _ := env.HostMount(e)
	guardCwd := t.resolveCommandCwd(ctx, workdir, base, sessionKey, kind, mountedHost, e)
	reader := func(p string) string { return t.readScriptForGuard(ctx, e, guardCwd, p, maxReferencedScriptBytes) }
	scan := lifecycleScan{t: t, ctx: ctx, det: det, readRemote: reader}
	unsafe, refusal := scan.run(command, guardCwd)
	if unsafe && refusal != "" {
		r := blockedJSON(fmt.Sprintf("Blocked: the lifecycle guard could not scan this command or referenced script: %s. ", refusal)+
			"Nothing in the command is known to contain a gateway lifecycle command, but a "+
			"script the command executes must be scannable (a regular text file under 1 MiB) "+
			"before it can run inside the gateway process.", "error")
		return &r, nil
	}
	if unsafe {
		if lifecycleScanRootWithinBudget(command) && det.HostImageKill(command) {
			r := blockedJSON(approval.HostImageKillRejection, "error")
			return &r, nil
		}
		r := blockedJSON("Blocked: command or referenced script cannot restart, stop, or "+
			"uninstall the gateway from inside the gateway process. The gateway would "+
			"kill this command before it could complete (SIGTERM propagates "+
			"to child processes). Run `icebath gateway restart` from a "+
			"separate shell outside the running gateway.", "error")
		return &r, nil
	}
	return nil, nil
}

func (t *Terminal) readScriptForGuard(ctx context.Context, e tool.Environment, guardCwd, scriptPath string, maxBytes int64) string {
	if e == nil {
		return ""
	}
	local := home.ExpandUser(scriptPath)
	if !filepath.IsAbs(local) {
		local = filepath.Join(guardCwd, local)
	}
	if info, err := os.Stat(local); err == nil && info.Mode().IsRegular() && info.Size() <= maxBytes {
		if data, err := os.ReadFile(local); err == nil && int64(len(data)) <= maxBytes {
			if bytes.IndexByte(data, 0) >= 0 {
				return ""
			}
			return strings.ToValidUTF8(string(data), replacementChar)
		}
	}
	res, err := e.Execute(ctx, fmt.Sprintf("head -c %d < %s", maxBytes+1, shell.Quote(scriptPath)), tool.ExecOptions{RewriteCompoundBackground: true})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	if strings.Contains(res.Output, "\x00") {
		return ""
	}
	return res.Output
}

func (t *Terminal) selfRepoBlock(ctx context.Context, command, cwd, workdir, sessionKey string) (*tool.Result, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	guardCwd := t.resolveCommandCwd(ctx, workdir, cwd, sessionKey, "", "", nil)
	msg, hit, err := t.detectSelfRepoMutation(ctx, command, guardCwd)
	if err != nil || !hit {
		return nil, err
	}
	slog.Warn(fmt.Sprintf("Blocked self-repo git mutation (command: %s)", commandPreview(command)))
	r := blockedJSON(msg, "blocked")
	return &r, nil
}

func (t *Terminal) lockedSkillBlock(ctx context.Context, command, cwd, workdir, sessionKey string) (*tool.Result, error) {
	guardCwd := t.resolveCommandCwd(ctx, workdir, cwd, sessionKey, "", "", nil)
	reason, locked, err := t.o.SkillLocks.CommandRefusal(ctx, command, guardCwd)
	if err != nil || !locked {
		return nil, err
	}
	slog.Warn(fmt.Sprintf("Blocked write to a locked skill (command: %s)", commandPreview(command)))
	r := blockedJSON("Blocked: "+reason, "blocked")
	return &r, nil
}

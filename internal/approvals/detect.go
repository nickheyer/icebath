package approvals

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	sshPath          = `(?:~|\$home|\$\{home\})/\.ssh(?:/|$)`
	icebathPrefix    = `(?:~\/\.icebath/|(?:\$home|\$\{home\})/\.icebath/|(?:\$icebath_home|\$\{icebath_home\}|\$hermes_home|\$\{hermes_home\})/)`
	icebathEnv       = icebathPrefix + `\.env\b`
	icebathConfig    = icebathPrefix + `config\.yaml\b`
	projectEnv       = `(?:(?:/|\.{1,2}/)?(?:[^\s/"'` + "`" + `]+/)*\.env(?:\.[^/\s"'` + "`" + `]+)*)`
	projectConfig    = `(?:(?:/|\.{1,2}/)?(?:[^\s/"'` + "`" + `]+/)*config\.yaml)`
	shellRcFiles     = `(?:~|\$home|\$\{home\})/\.(?:bashrc|zshrc|profile|bash_profile|zprofile)\b`
	credentialFiles  = `(?:~|\$home|\$\{home\})/\.(?:netrc|pgpass|npmrc|pypirc)\b`
	homePrefix       = `(?:~|\$home|\$\{home\})/`
	secretFile       = `(?:` + sshPath + `|` + icebathEnv + `|` + credentialFiles + `|` + projectEnv + `(?![\w-])` + `|` + homePrefix + `\.(?:aws/credentials|git-credentials|config/gh/hosts\.yml|docker/config\.json|kube/config)\b` + `|/etc/(?:shadow|passwd)\b|\bid_(?:rsa|ed25519|ecdsa|dsa)\b)`
	secretVar        = `\$\{?\w*(?:key|token|secret|passwd|password|credential)\w*\}?`
	uploadFlag       = `(?<!\S)(?:(?-i:-[a-zA-Z]*[dFT])|--data(?:-binary|-raw|-urlencode|-ascii)?|--form(?:-string)?|--upload-file|--json|--post-(?:data|file)|--body-(?:data|file))`
	emojiRange       = `\U0001F000-\U0001FAFF☀-➿`
	emojiBases       = `©®‼⁉™ℹ↔-↙↩↪⌚⌛⌨⏏⏩-⏳⏸-⏺Ⓜ▪▫▶◀◻-◾⤴⤵⬅-⬇⬛⬜⭐⭕〰〽㊗㊙` + emojiRange
	globalFlags      = `(?:-\S++(?:\s++(?!-\S)\S++)?\s++)*`
	containerFlags   = `(?:-\S++(?:\s(?!-\S)\S++)?\s++)*`
	systemConfig     = `(?:/etc/|/private/(?:etc|var|tmp|home)/)`
	sensitiveTarget  = `(?:` + systemConfig + `|/dev/sd|` + sshPath + `|` + icebathEnv + `|` + icebathConfig + `|` + shellRcFiles + `|` + credentialFiles + `)`
	userSensitive    = `(?:` + sshPath + `|` + shellRcFiles + `|` + credentialFiles + `)`
	projectSensitive = `(?:` + projectEnv + `|` + projectConfig + `)`
	commandTail      = `(?:\s*(?:&&|\|\||;).*)?$`
	targetBound      = `(?=[\s;&|<>"']|$)`
	cmdPos           = `(?:^|[\n` + "`" + `]|\$\()` + `\s*` + `(?:sudo\s+(?:-[^\s]+\s+)*)?` + `(?:env\s+(?:\w+=\S*\s+)*)?` + `(?:(?:exec|nohup|setsid|time)\s+)*` + `\s*`
	systemDirs       = `/home|/home/\*|/root|/root/\*|/etc|/etc/\*|/usr|/usr/\*|/var|/var/\*|/bin|/bin/\*|/sbin|/sbin/\*|/boot|/boot/\*|/lib|/lib/\*`
	rmFlags          = cmdPos + `rm\s+(-[^\s]*\s+)*`
	pkgOpts          = `(?:-[^\s]+(?:\s+[^-\s][^\s]*)?\s+)*`
	shells           = `bash|sh|zsh|ksh|dash`
)

// Descriptions the rules give, which are also the keys their answers are remembered under
const (
	invisibleUnicode = "invisible or bidirectional Unicode control character (possible obfuscation)"
	parserLimit      = "command parser limit exceeded"
	malformedExec    = "command parser limit or malformed executable payload"
	lifecycleSplice  = "stop/restart icebath gateway via shell-spliced verb (kills running agents)"
)

// Longest commands, and longest without a separator, the rules read; and most separators
const (
	maxCommandChars      = 128000
	maxUnseparatedChars  = 4096
	maxCommandSeparators = 25000
)

// Path a destructive rm names, quoted whole or bare up to what ends it
func rmPath(alternatives string) string {
	return `(?:["'](?:` + alternatives + `)["']|(?:` + alternatives + `)(?:\s|$|[)` + "`" + `;|&]))`
}

// One rule: what it matches and what the command is then flagged as
type rule struct {
	pattern     string
	description string
}

// Commands never run through the agent, whatever yolo or the approval mode say
var hardlineRules = []rule{
	{rmFlags + rmPath(`/(?:(?:\.\.?)?/)*(?:\.\.?)?\**|/ \*`), "recursive delete of root filesystem"},
	{rmFlags + rmPath(systemDirs), "recursive delete of system directory"},
	{rmFlags + rmPath(`(?:~|\$\{?HOME\}?)(?:/?|/\*)?`), "recursive delete of home directory"},
	{cmdPos + `mkfs(\.[a-z0-9]+)?\b`, "format filesystem (mkfs)"},
	{cmdPos + `dd\b[^\n]*\bof=/dev/(sd|nvme|hd|mmcblk|vd|xvd)[a-z0-9]*`, "dd to raw block device"},
	{`>\s*/dev/(sd|nvme|hd|mmcblk|vd|xvd)[a-z0-9]*\b`, "redirect to raw block device"},
	{`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`, "fork bomb"},
	{cmdPos + `kill\s+(-[^\s]+\s+)*-1\b`, "kill all processes"},
	{cmdPos + `(shutdown|reboot|halt|poweroff)\b`, "system shutdown/reboot"},
	{cmdPos + `init\s+[06]\b`, "init 0/6 (shutdown/reboot)"},
	{cmdPos + `systemctl\s+(poweroff|reboot|halt|kexec)\b`, "systemctl poweroff/reboot"},
	{cmdPos + `telinit\s+[06]\b`, "telinit 0/6 (shutdown/reboot)"},
}

// Hardline rules matched with quoted prose blanked, since they match anywhere in the text
var hardlineMasked = strs.Set("redirect to raw block device", "fork bomb")

// Commands that ask before they run
var dangerousRules = []rule{
	{`\brm\s+(-[^\s]*\s+)*/`, "delete in root path"},
	{`\brm\s+-[^\s]*r`, "recursive delete"},
	{`\brm\s+--recursive\b`, "recursive delete (long flag)"},
	{`\brm\s+(?!--(?:\s|$))(?:(?!\s--(?:\s|$))[^\n"';|&])*\s(?:-[a-z]*r[a-z]*\b|--recursive\b)`, "recursive delete (flags after operands)"},
	{`\bcmd(?:\.exe)?\s+/(?:c|k)\s+.*\b(?:del|erase|rd|rmdir)\b`, "Windows cmd destructive delete"},
	{`\b(?:powershell|pwsh)(?:\.exe)?\b(?:\s+-\S+)*\s+(?:-(?:command|c)\s+)?["']?(?:remove-item|rmdir|erase|del|rd|ri|rm)\b`, "Windows PowerShell destructive delete"},
	{`\b(?:powershell|pwsh)(?:\.exe)?\b.*\s-(?:encodedcommand|enc|e)\b`, "PowerShell encoded command execution"},
	{`\bremove-item\b[^\n;|&]*\s-(?:recurse|force)\b`, "PowerShell destructive delete (Remove-Item)"},
	{`\b(?:del|erase|rd|rmdir)\s+(?:/[a-z]\s+)*/[sq]\b`, "Windows destructive delete (recursive/quiet switch)"},
	{`\b(?:iwr|invoke-webrequest|invoke-restmethod|irm|curl|wget)\b[^\n]*\|\s*(?:iex|invoke-expression)\b`, "pipe remote content to PowerShell (iwr | iex)"},
	{`\b(?:iex|invoke-expression)\s*\(\s*(?:iwr|invoke-webrequest|invoke-restmethod|irm)\b`, "execute remote content via Invoke-Expression"},
	{`\btaskkill\b[^\n]*\s/f\b`, "force kill processes (taskkill /F)"},
	{`\bstop-process\b[^\n]*\s-force\b`, "force kill processes (Stop-Process -Force)"},
	{`\bformat-volume\b`, "format filesystem (Format-Volume)"},
	{`\bclear-disk\b`, "wipe disk (Clear-Disk)"},
	{`\bdiskpart\b`, "disk partitioning (diskpart)"},
	{`\bformat(?:\.com)?\s+[a-z]:`, "format drive (format.com)"},
	{`\bcipher\s+/w\b`, "wipe free space (cipher /w)"},
	{`\bicacls\b[^\n]*\s/grant\b[^\n]*\b(?:everyone|todos|jeder|tout\s+le\s+monde|\*s-1-1-0)\b`, "grant Everyone access (icacls)"},
	{`\bicacls\b[^\n]*\s/reset\b`, "reset ACLs recursively (icacls /reset)"},
	{`\bvssadmin\b[^\n]*\bdelete\s+shadows\b`, "delete volume shadow copies (vssadmin)"},
	{`\bwbadmin\b[^\n]*\bdelete\b`, "delete backups (wbadmin)"},
	{`\bbcdedit\b[^\n]*\s/set\b`, "modify boot configuration (bcdedit /set)"},
	{`\breg(?:\.exe)?\s+delete\b`, "registry delete (reg delete)"},
	{`\bremove-itemproperty\b[^\n]*\s-force\b`, "registry value delete (Remove-ItemProperty -Force)"},
	{`\bstop-service\b[^\n]*\s-force\b`, "force stop service (Stop-Service -Force)"},
	{`\bsc(?:\.exe)?\s+(?:stop|delete)\b`, "stop/delete service (sc)"},
	{`\busers[\\/][^\\/\s]+[\\/]\.ssh\b`, "access to SSH keys (Windows path)"},
	{`\bappdata[\\/](?:local|roaming)[\\/]icebath[^\n]*\.env\b`, "access to icebath secrets (Windows path)"},
	{`\bchmod\s+(-[^\s]*\s+)*(777|666|o\+[rwx]*w|a\+[rwx]*w)\b`, "world/other-writable permissions"},
	{`\bchmod\s+--recursive\b.*(777|666|o\+[rwx]*w|a\+[rwx]*w)`, "recursive world/other-writable (long flag)"},
	{`\bchown\s+(-[^\s]*)?R\s+root`, "recursive chown to root"},
	{`\bchown\s+--recur[a-z]*\b.*root`, "recursive chown to root (long flag)"},
	{cmdPos + `mkfs\b`, "format filesystem"},
	{cmdPos + `dd\s+.*if=`, "disk copy"},
	{`>\s*/dev/sd`, "write to block device"},
	{`\bDROP\s+(TABLE|DATABASE)\b`, "SQL DROP"},
	{`\bDELETE\s+FROM\b(?![^\n]*\bWHERE\b)`, "SQL DELETE without WHERE"},
	{`\bTRUNCATE\s+(TABLE)?\s*\w`, "SQL TRUNCATE"},
	{`>\s*` + systemConfig, "overwrite system config"},
	{`\bsystemctl\s+(-[^\s]+\s+)*(stop|restart|disable|mask)\b`, "stop/restart system service"},
	{`\bkill\s+-9\s+-1\b`, "kill all processes"},
	{`\bpkill\s+-9\b`, "force kill processes"},
	{`\bkillall\s+(-[^\s]*\s+)*-(9|KILL|SIGKILL)\b`, "force kill processes (killall -KILL)"},
	{`\bkillall\s+(-[^\s]*\s+)*-s\s+(KILL|SIGKILL|9)\b`, "force kill processes (killall -s KILL)"},
	{`\bkillall\s+(-[^\s]*\s+)*-r\b`, "kill processes by regex (killall -r)"},
	{`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`, "fork bomb"},
	{`\b(curl|wget)\b.*\|\s*(?:[/\w]*/)?(?:` + shells + `)(?:\s|$|-c)`, "pipe remote content to shell"},
	{`\b(?:` + shells + `)\s+<\s*<?\s*\(\s*(curl|wget)\b`, "execute remote script via process substitution"},
	{`(?:\beval\b|\bsource\b|\.)\s*(?:\$\(\s*|` + "`" + `\s*)(?:curl|wget)\b`, "execute remote content via command substitution"},
	{`(?<![\d.])(?:169\.254\.169\.254|100\.100\.100\.200)(?![\d.])|(?<![\w.-])metadata\.google\.internal(?![\w.-])|fd00:ec2::254`, "cloud metadata endpoint access (instance credentials)"},
	{`\b(base64|base32|base16)\s+(?:-[dD]|--decode)\b.*\|\s*\b(?:` + shells + `)\b`, "pipe decoded content to shell (possible command obfuscation)"},
	{`\bxxd\s+-r\b.*\|\s*\b(?:` + shells + `)\b`, "pipe xxd-decoded content to shell (possible command obfuscation)"},
	{`\becho\b[^|]*\|\s*\btr\b[^|]*\|\s*\b(?:` + shells + `)\b`, "pipe tr-transformed output to shell (possible command obfuscation)"},
	{`\bopenssl\b.*\b(?:base64|enc)\b[^|]*\s+-[dD]\b[^|]*\|\s*\b(?:` + shells + `)\b`, "pipe openssl-decoded content to shell (possible command obfuscation)"},
	{uploadFlag + `(?:[\s=]+|(?=[@"'$]))(?:"[^"]*(?:` + secretVar + `|` + secretFile + `)|'[^']*` + secretFile + `|\$\([^)]*` + secretFile + `|\S*?(?:` + secretVar + `|` + secretFile + `))`, "upload a secret or credential file via curl/wget (possible exfiltration)"},
	{secretFile + `[^|;&\n]*\|[^;&|\n]*\b(?:curl|wget)\b[^;&|\n]*` + uploadFlag, "pipe a credential file into a curl/wget upload (possible exfiltration)"},
	{`[\u200b\u200c\u200e\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff\u180e\U000e0000-\U000e007f]|(?<![` + emojiRange + `\ufe0f])\u200d|\u200d(?![` + emojiRange + `])`, invisibleUnicode},
	{`\btee\b.*["']?` + sensitiveTarget, "overwrite system file via tee"},
	{`>>?\s*["']?` + sensitiveTarget, "overwrite system file via redirection"},
	{`\btee\b.*["']?` + projectSensitive + `["']?` + targetBound, "overwrite project env/config via tee"},
	{`>>?\s*["']?` + projectSensitive + `["']?` + targetBound, "overwrite project env/config via redirection"},
	{`\bxargs\s+.*\brm\b`, "xargs with rm"},
	{`\bfind\b.*-exec(?:dir)?\s+(/\S*/)?rm\b`, "find -exec/-execdir rm"},
	{cmdPos + `find\s[^;|&\n]*(?<!\S)-(?:\{[^}\s]*(?:delete|exec(?:dir)?)[^}\s]*\}|(?:del(?:ete?)?|exec(?:dir)?)[*?\[])`, "find dynamic shell word may expand to destructive flag"},
	{`\bfind\b.*-delete\b`, "find -delete"},
	{`\b(?:rg|sort|ag|man)\b[^;|&\n]*(?<!\S)--(?:pre|hostname-bin|compress-program|pager|html)(?:\{|[*?\[])`, "dynamic shell word may expand to arbitrary program execution flag"},
	{`\bicebath\s+` + globalFlags + `gateway\s+(stop|restart)\b`, "stop/restart icebath gateway (kills running agents)"},
	{`\bicebath\s+update\b`, "icebath update (restarts gateway, kills running agents)"},
	{`\bdocker\s+` + containerFlags + `(?:-h|--host)[=\s]+\S+`, "docker with remote daemon redirect (-H/--host)"},
	{`\bdocker\s+` + containerFlags + `(?:-c|--context)[=\s]+\S+`, "docker with daemon redirect (--context: alternate daemon)"},
	{`\bdocker\s+context\s+use\b`, "docker context use (switches default daemon for future commands)"},
	{`\bpodman\s+` + containerFlags + `(?:--url|--connection|--identity)[=\s]+\S+`, "podman with remote daemon redirect (--url/--connection/--identity)"},
	{`\bpodman\s+` + containerFlags + `(?:-r\b|--remote\b)`, "podman remote mode (-r/--remote: remote daemon)"},
	{`\b(?:docker_host|docker_context|container_host|container_connection)=\S+`, "docker/podman daemon redirect via environment (DOCKER_HOST/CONTAINER_HOST)"},
	{`\bdocker(?:-compose|\s+compose)\s+` + containerFlags + `(restart|stop|kill|down)\b`, "docker compose restart/stop/kill/down (container lifecycle)"},
	{`\bdocker\s+` + containerFlags + `(restart|stop|kill)\b`, "docker restart/stop/kill (container lifecycle)"},
	{`gateway\s+run\b.*(&\s*$|&\s*;|\bdisown\b|\bsetsid\b)`, "start gateway outside systemd (use 'systemctl --user restart icebath-gateway')"},
	{`\bnohup\b.*gateway\s+run\b`, "start gateway outside systemd (use 'systemctl --user restart icebath-gateway')"},
	{`\b(pkill|killall)\b.*\b(icebath|gateway|cli\.py)\b`, "kill icebath/gateway process (self-termination)"},
	{`\bkill\b.*\$\(\s*(pgrep|pidof)\b`, "kill process via pgrep/pidof expansion (self-termination)"},
	{`\bkill\b.*` + "`" + `\s*(pgrep|pidof)\b`, "kill process via backtick pgrep/pidof expansion (self-termination)"},
	{`\A(?=[\s\S]*\blaunchctl\s+(?:stop|kickstart|bootout|unload|kill|disable|remove)\b)(?=[\s\S]*\b(?:icebath|ai\.icebath)\b)`, "stop/restart icebath launchd service (kills running agents)"},
	{`\b(cp|mv|install)\b.*\s` + systemConfig, "copy/move file into system config path"},
	{`\b(cp|mv|install)\b.*\s["']?` + projectSensitive + `["']?` + commandTail, "overwrite project env/config file"},
	{`\b(cp|mv|install)\b.*\s["']?` + sensitiveTarget + `[^\s"']*["']?` + commandTail, "copy/move file into sensitive credential/SSH/shell-rc path"},
	{`\bsed\s+-[^\s]*i.*(?:` + userSensitive + `)[^\s"']*`, "in-place edit of sensitive credential/SSH/shell-rc path"},
	{`\bsed\s+--in-place\b.*(?:` + userSensitive + `)[^\s"']*`, "in-place edit of sensitive credential/SSH/shell-rc path (long flag)"},
	{`\b(?:perl|ruby)\b.*(?:^|\s)-[^\s]*i\b.*(?:` + userSensitive + `)[^\s"']*`, "in-place edit of sensitive credential/SSH/shell-rc path (perl/ruby)"},
	{`\bsed\s+-[^\s]*i.*\s` + systemConfig, "in-place edit of system config"},
	{`\bsed\s+--in-place\b.*\s` + systemConfig, "in-place edit of system config (long flag)"},
	{`\bsed\s+-[^\s]*i.*(?:` + icebathConfig + `|` + icebathEnv + `)`, "in-place edit of icebath config/env"},
	{`\bsed\s+--in-place\b.*(?:` + icebathConfig + `|` + icebathEnv + `)`, "in-place edit of icebath config/env (long flag)"},
	{`\b(?:perl|ruby)\b.*(?:^|\s)-[^\s]*i\b.*(?:` + icebathConfig + `|` + icebathEnv + `)`, "in-place edit of icebath config/env (perl/ruby)"},
	{`\b(?:` + shells + `)\s+<<`, "shell execution via heredoc"},
	{`\bgit\s+reset\s+--h(?:a(?:r(?:d)?)?)?\b`, "git reset --hard (destroys uncommitted changes)"},
	{`\bgit\s+push\b.*--forc[a-z]*\b`, "git force push (rewrites remote history)"},
	{`\bgit\s+push\b.*-f\b`, "git force push short flag (rewrites remote history)"},
	{`\bgit\s+clean\s+-[^\s]*f`, "git clean with force (deletes untracked files)"},
	{`\bgit\s+branch\s+(?-i:-D)\b`, "git branch force delete"},
	{`\bgit\s+branch\b[^;|&\n]*?(?:-d\b|--delete\b)[^;|&\n]*?(?:-f\b|--force\b)`, "git branch force delete (long flags)"},
	{`\bgit\s+branch\b[^;|&\n]*?(?:-f\b|--force\b)[^;|&\n]*?(?:-d\b|--delete\b)`, "git branch force delete (long flags, force-first)"},
	{`\bchmod\s+\+x\b.*[;&|]+\s*\./`, "chmod +x followed by immediate execution"},
	{`\bsudo\b[^;|&\n]*?\s+(?:-s\b|--st[a-z]*\b|-a\b|--a[a-z]*\b)`, "sudo with privilege flag (stdin/askpass/shell/list)"},
	{`\bsudo\b[^;|&\n]*?\s+-[a-z]*[sa][a-z]*\b`, "sudo with combined-flag privilege escalation"},
	{cmdPos + `npm\s+` + pkgOpts + `(?:uninstall|unlink|remove|rm|r|un)\b`, "package manager uninstall"},
	{cmdPos + `pnpm\s+` + pkgOpts + `(?:uninstall|remove|rm|un)\b`, "package manager uninstall"},
	{cmdPos + `yarn\s+` + pkgOpts + `(?:global\s+)?(?:uninstall|remove)\b`, "package manager uninstall"},
	{cmdPos + `pip(?:3)?\s+` + pkgOpts + `uninstall\b`, "package manager uninstall"},
	{cmdPos + `brew\s+` + pkgOpts + `(?:uninstall|remove|rm)\b`, "package manager uninstall"},
}

// Dangerous rules matched with quoted prose blanked
var dangerousMasked = strs.Set("find dynamic shell word may expand to destructive flag", "dynamic shell word may expand to arbitrary program execution flag")

// Rules about what a command carries rather than the command that runs, each asked about on its own
var contentRules = strs.Set(
	"upload a secret or credential file via curl/wget (possible exfiltration)",
	"pipe a credential file into a curl/wget upload (possible exfiltration)",
	invisibleUnicode,
)

type compiled struct {
	re          *regexp2.Regexp
	description string
}

var unicodeEscape = regexp.MustCompile(`\\U[0-9a-fA-F]{8}|\\u[0-9a-fA-F]{4}`)

// Pattern in the syntax regexp2 compiles: possessive runs as atomic groups, \u and \U escapes as the characters
func dialect(src string) string {
	s := strings.ReplaceAll(src, `\S++`, `(?>\S+)`)
	s = strings.ReplaceAll(s, `\s++`, `(?>\s+)`)
	s = strings.ReplaceAll(s, `(?P<`, `(?<`)
	return unicodeEscape.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

func compile(rules []rule) []compiled {
	out := make([]compiled, len(rules))
	for i, r := range rules {
		out[i] = compiled{regexp2.MustCompile(dialect(r.pattern), regexp2.IgnoreCase|regexp2.Singleline), r.description}
	}
	return out
}

var (
	hardline  = compile(hardlineRules)
	dangerous = compile(dangerousRules)
	// Variation selectors that show nothing, except after the emoji and keycaps they belong to
	hiddenSelectors = regexp2.MustCompile(dialect(`(?<![`+emojiBases+`])(?!(?<=[0-9#*])\ufe0f\u20e3)[\ufe00-\ufe0f\U000e0100-\U000e01ef]`), regexp2.None)
)

func matches(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// Every description the dangerous rules give, once each, which the allowlist can hold
func Kinds() []string {
	var out []string
	for _, r := range dangerousRules {
		if !slices.Contains(out, r.description) {
			out = append(out, r.description)
		}
	}
	return out
}

// Pattern of one path as an absolute prefix of a word, its tail captured
const pathTail = `(?P<tail>(?:[/\\][^/\\\s'"` + "`" + `;|&<>()]*)+)`

var separators = regexp.MustCompile(`[/\\]+`)

// Fold of one spelling of a home folder into the form the rules match
type fold struct {
	re          *regexp.Regexp
	replacement string
}

// Reader of commands for one profile: the folders of icebath and of the user, spelled out, are folded to the ~ forms the rules match
type reader struct {
	folds []fold
	// Profile the commands run on, which a lifecycle command naming another profile leaves alone
	profile string
}

func newReader(profile string, icebath []string) *reader {
	r := &reader{profile: profile}
	r.add(icebath, "~/.icebath")
	if home, err := os.UserHomeDir(); err == nil {
		real := home
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			real = resolved
		}
		r.add([]string{home, real, os.Getenv("HOME")}, "~")
	}
	return r
}

// Folds for the folders, the longest first
func (r *reader) add(paths []string, replacement string) {
	sorted := slices.Clone(paths)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	seen := map[string]bool{}
	for _, p := range sorted {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		var parts []string
		for _, part := range separators.Split(p, -1) {
			if part != "" {
				parts = append(parts, regexp.QuoteMeta(part))
			}
		}
		if len(parts) >= 2 {
			r.folds = append(r.folds, fold{regexp.MustCompile(`[/\\]*` + strings.Join(parts, `[/\\]+`) + pathTail), replacement})
		}
	}
}

var (
	lineContinuation = regexp.MustCompile(`\\\r?\n`)
	escaped          = regexp.MustCompile(`\\([^\n])`)
	ifs              = regexp.MustCompile(`\$\{IFS\b[^}]*\}|\$IFS\b`)
	emptyLiterals    = strings.NewReplacer("''", "", `""`, "")
)

// Command spelled so escapes, full width letters, spelled out homes and splicing can't hide what it does
func (r *reader) normalize(command string) string {
	command = norm.NFKC.String(strings.ReplaceAll(term.StripANSI(command), "\x00", ""))
	command = lineContinuation.ReplaceAllString(command, "")
	for _, f := range r.folds {
		tail := f.re.SubexpIndex("tail")
		command = f.re.ReplaceAllStringFunc(command, func(m string) string {
			return f.replacement + strings.ReplaceAll(f.re.FindStringSubmatch(m)[tail], `\`, "/")
		})
	}
	command = escaped.ReplaceAllString(command, "${1}")
	command = emptyLiterals.Replace(command)
	return ifs.ReplaceAllString(command, " ")
}

// Command lowered save for words starting with a dash, whose case can matter
func lowerKeepingFlags(command string) string {
	var b strings.Builder
	for i := 0; i < len(command); {
		r, size := runeAt(command, i)
		j := i + size
		inSpace := unicode.IsSpace(r)
		for j < len(command) {
			r2, s2 := runeAt(command, j)
			if unicode.IsSpace(r2) != inSpace {
				break
			}
			j += s2
		}
		if word := command[i:j]; inSpace || strings.HasPrefix(word, "-") {
			b.WriteString(word)
		} else {
			b.WriteString(strings.ToLower(word))
		}
		i = j
	}
	return b.String()
}

// Whether the command is too long or split too often to read safely
func tooLarge(command string) bool {
	n := utf8.RuneCountInString(command)
	if n > maxCommandChars || n > maxUnseparatedChars && !strings.ContainsAny(command, ";&|\n") {
		return true
	}
	return strings.Count(command, ";")+strings.Count(command, "&")+strings.Count(command, "|")+strings.Count(command, "\n") >= maxCommandSeparators
}

// Interpreter names by family
var interpreters = []struct {
	family string
	re     *regexp.Regexp
}{
	{"python", regexp.MustCompile(`\A(?:py(?:\.exe)?|python[23]?(?:\.\d+)*(?:\.exe)?)\z`)},
	{"node", regexp.MustCompile(`\A(?:node(?:js)?(?:\.exe)?)\z`)},
	{"perl", regexp.MustCompile(`\A(?:perl[0-9]*(?:\.\d+)*(?:\.exe)?)\z`)},
	{"ruby", regexp.MustCompile(`\A(?:ruby[0-9.]*(?:\.exe)?)\z`)},
	{"php", regexp.MustCompile(`\A(?:php(?:\.exe)?)\z`)},
	{"powershell", regexp.MustCompile(`\A(?:powershell(?:\.exe)?|pwsh(?:\.exe)?)\z`)},
	{"bun", regexp.MustCompile(`\A(?:bun(?:\.exe)?)\z`)},
	{"deno", regexp.MustCompile(`\A(?:deno(?:\.exe)?)\z`)},
}

// Options that run code given on the command line, by family
var execFlags = map[string]map[string]bool{
	"python": strs.Set("-c"), "node": strs.Set("-e", "--eval", "-p", "--print"), "perl": strs.Set("-e", "--eval"), "ruby": strs.Set("-e"),
	"php": strs.Set("-r"), "powershell": strs.Set("-command", "-c", "-file", "-f"), "bun": strs.Set("-e", "--eval"), "deno": strs.Set("eval", "-e", "--eval"),
}

// Interpreter options that take a value, by family
var interpreterArgOptions = map[string]map[string]bool{
	"python": strs.Set("-W", "-X", "--check-hash-based-pycs"),
	"node": strs.Set("-C", "--conditions", "--cpu-prof-dir", "--diagnostic-dir", "--icu-data-dir", "--import", "--loader", "--openssl-config",
		"--require", "--title"),
	"perl": strs.Set("-0", "-F", "-I", "-M", "-m", "-x"), "ruby": strs.Set("-C", "-E", "-F", "-I", "-K", "-r"), "php": strs.Set("-c", "-d", "-z"),
	"powershell": strs.Set("-configurationname", "-custompipename", "-executionpolicy", "-inputformat", "-outputformat", "-settingsfile",
		"-version", "-windowstyle", "-workingdirectory"),
	"bun": strs.Set("--config", "--cwd", "--env-file", "--preload", "--require"), "deno": strs.Set("-L", "--log-level"),
}

// Options of read only tools that run another program
var programFlags = map[string]map[string]bool{
	"sort": strs.Set("--compress-program"), "rg": strs.Set("--pre", "--hostname-bin"), "ag": strs.Set("--pager"), "man": strs.Set("--pager", "--html", "-P", "-H"),
}

var programLongArgOptions = map[string]map[string]bool{
	"rg": strs.Set("--after-context", "--before-context", "--color", "--colors", "--context", "--context-separator", "--dfa-size-limit", "--encoding",
		"--engine", "--field-context-separator", "--field-match-separator", "--file", "--generate", "--glob", "--hostname-bin",
		"--hyperlink-format", "--iglob", "--ignore-file", "--max-columns", "--max-count", "--max-depth", "--max-filesize", "--path-separator",
		"--pre", "--pre-glob", "--regex-size-limit", "--regexp", "--replace", "--sort", "--sortr", "--threads", "--type", "--type-add",
		"--type-clear", "--type-not"),
	"sort": strs.Set("--batch-size", "--buffer-size", "--compress-program", "--field-separator", "--files0-from", "--key", "--output", "--parallel",
		"--random-source", "--sort", "--temporary-directory"),
	"man": strs.Set("--config-file", "--encoding", "--extension", "--locale", "--manpath", "--pager", "--preprocessor", "--prompt", "--recode",
		"--sections", "--systems"),
	"ag": strs.Set("--ackmate-dir-filter", "--color-line-number", "--color-match", "--color-path", "--depth", "--filename-pattern",
		"--file-search-regex", "--ignore", "--ignore-dir", "--max-count", "--pager", "--path-to-ignore", "--width", "--workers"),
}

var programShortArgLetters = map[string]string{"rg": "efEmjgdtTABCMr", "sort": "koStT", "man": "CRLmMSserEPp", "ag": "gGmpW"}

var (
	bashArgOptions = strs.Set("-O", "+O", "-o", "+o", "--init-file", "--rcfile")
	bashLetters    = "ilrsDcabefhkmnptuvxBCEHPTOo"
	shellNames     = strs.Set("bash", "sh", "zsh", "ksh", "dash")
	// Commands that run shell text they are given
	carriers = strs.Set("eval", "sh", "bash", "zsh", "ksh", "dash", "source", ".")
)

func family(executable string) string {
	name := strings.ToLower(base(executable))
	for _, entry := range interpreters {
		if entry.re.MatchString(name) {
			return entry.family
		}
	}
	return ""
}

// Interpreter option that runs code from the command line, empty when none is given
func execFlag(fam string, args []string) string {
	flags, withArg := execFlags[fam], interpreterArgOptions[fam]
	powershell := fam == "powershell"
	skip := false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		if arg == "--" || !powershell && !strings.HasPrefix(arg, "-") {
			if fam == "deno" && strings.ToLower(arg) == "eval" {
				return "eval"
			}
			break
		}
		option, _, equals := strings.Cut(arg, "=")
		comparable := option
		if powershell {
			comparable = strings.ToLower(option)
		}
		if flags[comparable] {
			return comparable
		}
		attached := false
		for short := range withArg {
			if strings.HasPrefix(short, "-") && !strings.HasPrefix(short, "--") && strings.HasPrefix(option, short) && len(option) > len(short) {
				attached = true
				break
			}
		}
		if !powershell && !strings.HasPrefix(option, "--") && utf8.RuneCountInString(option) > 2 && !attached {
			for _, letter := range option[1:] {
				if flags["-"+string(letter)] {
					return "-" + string(letter)
				}
			}
		}
		skip = withArg[comparable] && !equals
	}
	return ""
}

func onlyLetters(s, allowed string) bool {
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}

// Whether a shell is given -c, and the text that -c runs when there is any
func dashC(args []string) (found bool, payload string, has bool) {
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
			break
		}
		if bashArgOptions[arg] {
			i += 2
			continue
		}
		letters := arg[1:]
		if strings.HasPrefix(arg, "--") || !onlyLetters(letters, bashLetters) {
			i++
			continue
		}
		consumed := 0
		if strings.ContainsAny(letters, "Oo") {
			consumed = 1
		}
		if strings.Contains(letters, "c") {
			if at := i + 1 + consumed; at < len(args) {
				return true, args[at], true
			}
			return true, "", false
		}
		i += 1 + consumed
	}
	return false, "", false
}

// Option of a read only tool that runs a program, and the program
func programFlag(tool string, args []string) (string, string, bool) {
	flags := programFlags[tool]
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" {
			break
		}
		option, payload, has := strings.Cut(arg, "=")
		matched := ""
		if flags[option] {
			matched = option
		}
		if tool == "man" && (strings.HasPrefix(arg, "-P") || strings.HasPrefix(arg, "-H")) && utf8.RuneCountInString(arg) > 2 {
			matched, payload, has = arg[:2], arg[2:], true
		}
		switch {
		case matched != "":
			if !has && i+1 < len(args) {
				payload, has = args[i+1], true
			}
			if payload != "" {
				return matched, payload, true
			}
			if has && !strings.Contains(arg, "=") {
				i += 2
			} else {
				i++
			}
		case programLongArgOptions[tool][option] && !has:
			i += 2
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && utf8.RuneCountInString(arg) > 1:
			owner, k := -1, 1
			for _, letter := range arg[1:] {
				if strings.ContainsRune(programShortArgLetters[tool], letter) {
					owner = k
					break
				}
				k++
			}
			if owner == utf8.RuneCountInString(arg)-1 {
				i += 2
			} else {
				i++
			}
		default:
			i++
		}
	}
	return "", "", false
}

// Way a command runs code it carries, and that code when it is there
type finding struct {
	description string
	payload     string
	has         bool
}

// Ways the command runs code it carries: interpreter flags and heredocs, shell -c, programs read only tools run
func execFindings(command string) []finding {
	var out []finding
	for _, segment := range topSegments(command) {
		for _, ws := range commandWords(segment) {
			executable := plainWord(ws.word)
			name := strings.ToLower(base(executable))
			fam := family(executable)
			tokens, ok := segmentTokens(segment, ws.start)
			if !ok {
				if fam != "" || programFlags[name] != nil {
					out = append(out, finding{description: malformedExec})
				}
				continue
			}
			if len(tokens) == 0 {
				continue
			}
			args := tokens[1:]
			heredoc := slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "<<") })
			switch {
			case fam != "" && execFlag(fam, args) != "":
				out = append(out, finding{description: "script execution via -e/-c flag"})
			case fam != "" && heredoc:
				out = append(out, finding{description: "script execution via heredoc"})
			default:
				if shellNames[name] {
					if found, payload, has := dashC(args); found {
						out = append(out, finding{"shell command via -c/-lc flag", payload, has})
					}
				}
				if programFlags[name] != nil {
					if option, program, ok := programFlag(name, args); ok {
						out = append(out, finding{"arbitrary program execution via " + name + " " + option, program, true})
					}
				}
			}
		}
	}
	return out
}

var envEscapes = map[rune]string{'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v", '#': "#", '$': "$", '"': "\"", '\'': "'", '\\': "\\"}

// Words GNU env -S makes of its text, false when it needs expansion to know
func splitEnvString(payload string) ([]string, bool) {
	runes := []rune(payload)
	args := []string{}
	var word strings.Builder
	var quote rune
	started := false
	for i := 0; i < len(runes); {
		char := runes[i]
		i++
		if char == '\\' {
			if i == len(runes) {
				return nil, false
			}
			escaped := runes[i]
			if quote == '\'' && escaped != '\'' && escaped != '\\' {
				word.WriteRune(char)
				started = true
				continue
			}
			i++
			if escaped == 'c' {
				if quote != 0 {
					return nil, false
				}
				break
			}
			if escaped == '_' {
				if quote == 0 {
					if started {
						args = append(args, word.String())
					}
					word.Reset()
					started = false
					continue
				}
				word.WriteString(" ")
				started = true
				continue
			}
			replacement, ok := envEscapes[escaped]
			if !ok {
				return nil, false
			}
			word.WriteString(replacement)
			started = true
			continue
		}
		if (char == '\'' || char == '"') && (quote == 0 || char == quote) {
			if quote == 0 {
				quote = char
			} else {
				quote = 0
			}
			started = true
			continue
		}
		if quote == 0 && strings.ContainsRune(" \t\n\r\v\f", char) {
			if started {
				args = append(args, word.String())
			}
			word.Reset()
			started = false
			continue
		}
		if quote == 0 && char == '#' && !started {
			break
		}
		if char == '$' && quote != '\'' {
			return nil, false
		}
		word.WriteRune(char)
		started = true
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		args = append(args, word.String())
	}
	return args, true
}

// Command line an env -S runs, empty when none
func envSplitCommand(tokens []string) string {
	for i := 1; i < len(tokens); {
		arg := tokens[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return ""
		}
		option, value, equals := strings.Cut(arg, "=")
		if option == "--split-string" || strings.HasPrefix(arg, "-S") {
			attached := len(arg) > 2
			if option == "--split-string" {
				attached = equals
			}
			payload := ""
			switch {
			case attached && option == "--split-string":
				payload = value
			case attached:
				payload = arg[2:]
			default:
				if i++; i < len(tokens) {
					payload = tokens[i]
				}
			}
			args, ok := splitEnvString(payload)
			if !ok {
				return ""
			}
			if i+1 < len(tokens) {
				args = append(args, tokens[i+1:]...)
			}
			return terminal.Join(args)
		}
		if !equals && wrapperArgOptions["env"][option] {
			i += 2
		} else {
			i++
		}
	}
	return ""
}

var (
	windowsPath  = regexp.MustCompile(`(?:[A-Za-z]:|\\\\)[\\\\]`)
	windowsDrive = regexp.MustCompile(`[A-Za-z]:\\`)
	scratchName  = regexp.MustCompile(`\Aicebath-(?:verify|ad-hoc)-[A-Za-z0-9_.-]+\z`)
)

// Every spelling of the command the rules read, in the order they read them
func (r *reader) variants(command string) []string {
	normalized := r.normalize(maskQuotedNewlines(command))
	safe, _ := grepSafe(normalized)
	seen := map[string]bool{safe: true}
	out := []string{safe}
	fresh := func(variant string) bool {
		if variant == "" || seen[variant] {
			return false
		}
		seen[variant] = true
		return true
	}
	if windowsPath.MatchString(command) || windowsDrive.MatchString(command) {
		if slashed := r.normalize(maskQuotedNewlines(strings.ReplaceAll(command, `\`, "/"))); fresh(slashed) {
			out = append(out, slashed)
		}
	}
	pending := []string{normalized}
	for len(pending) > 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, f := range execFindings(source) {
			if !f.has || !fresh(f.payload) {
				continue
			}
			out = append(out, f.payload)
			if marked := markStarts(f.payload, "\n"); marked != f.payload && fresh(marked) {
				out = append(out, marked)
			}
			pending = append(pending, f.payload)
		}
	}
	if marked := markStarts(safe, "\n"); marked != safe && fresh(marked) {
		out = append(out, marked)
	}
	if faithful := r.normalize(markStarts(maskQuotedNewlines(command), " \n")); fresh(faithful) {
		out = append(out, faithful)
	}
	var edits []edit
	for _, ws := range commandWords(normalized) {
		if plain := plainWord(ws.word); plain != "" && plain != ws.word {
			edits = append(edits, edit{ws.start, ws.end, plain})
		}
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end < edits[j].end
	})
	for len(edits) > 0 {
		var applied, carry []edit
		cursor := 0
		for _, e := range edits {
			if e.start < cursor {
				carry = append(carry, e)
			} else {
				applied = append(applied, e)
				cursor = e.end
			}
		}
		if variant := splice(normalized, applied); fresh(variant) {
			out = append(out, variant)
		}
		edits = carry
	}
	return out
}

// Spellings the always-deny patterns read: the detection ones and every executable with its arguments, wrappers and env -S peeled
func (r *reader) denyVariants(command string) []string {
	out := r.variants(command)
	pending := []string{command}
	seen := map[string]bool{}
	for len(pending) > 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[source] {
			continue
		}
		seen[source] = true
		for _, ws := range commandWords(source) {
			segment := commandAt(source, ws.start)
			executable := plainWord(ws.word)
			tail := ""
			if ws.end-ws.start <= len(segment) {
				tail = segment[ws.end-ws.start:]
			}
			var parts strings.Builder
			lastSpace := false
			for sc := scan(tail, 0, -1, scanOptions{}); ; {
				st, ok := sc.next()
				if !ok {
					break
				}
				if ch, _ := runeAt(tail, st.i); st.kind == stepChar && st.quote == 0 && unicode.IsSpace(ch) {
					if !lastSpace {
						parts.WriteByte(' ')
						lastSpace = true
					}
				} else {
					parts.WriteString(tail[st.i:st.j])
					lastSpace = false
				}
			}
			tail = parts.String()
			names := []string{executable}
			if b := base(executable); b != executable {
				names = append(names, b)
			}
			for _, name := range names {
				out = append(out, name+tail, r.normalize(name+tail))
			}
			if base(executable) == "env" {
				if tokens, ok := segmentTokens(segment, 0); ok && len(tokens) > 0 {
					if payload := envSplitCommand(tokens); payload != "" {
						pending = append(pending, payload)
					}
				}
			}
		}
		for _, f := range execFindings(source) {
			if f.has && f.payload != "" {
				pending = append(pending, f.payload)
			}
		}
	}
	return out
}

// Whether the command only removes one of icebath's own scratch scripts from the temp folder
func scratchCleanup(command string) bool {
	argv, err := terminal.Split(command)
	if err != nil || len(argv) != 3 || argv[0] != "rm" || argv[1] != "-f" {
		return false
	}
	operand := argv[2]
	temp := os.TempDir()
	if real, err := filepath.EvalSymlinks(temp); err == nil {
		temp = real
	}
	name := base(operand)
	return operand == filepath.Join(temp, name) && filepath.Dir(resolve(operand)) == temp && scratchName.MatchString(name)
}

// Whether a command position word is one that runs shell text it is given
func hasCarrier(command string) bool {
	for _, ws := range commandWords(command) {
		if carriers[strings.ToLower(base(plainWord(ws.word)))] {
			return true
		}
	}
	return false
}

var sudoStdin = regexp.MustCompile("(?i)(?:^|[;&|`\n]|&&|\\|\\||\\$\\()\\s*sudo\\s+-S\\b")

// Why the command is refused outright as sudo password guessing on stdin, empty when it isn't; only when no sudo password is set
func (r *reader) sudoGuess(command string, passwordSet bool) string {
	if !passwordSet && sudoStdin.MatchString(strings.ToLower(r.normalize(command))) {
		return "sudo password guessing via stdin (sudo -S)"
	}
	return ""
}

// Hardline rule the command matches, empty when none; these can never be allowed
func (r *reader) hardline(command string) string {
	if tooLarge(command) {
		return parserLimit
	}
	if _, malformed := grepSafe(maskQuotedNewlines(command)); malformed {
		return malformedExec
	}
	for _, variant := range r.variants(command) {
		lower := strings.ToLower(variant)
		masked, done := "", false
		for _, p := range hardline {
			target := lower
			if hardlineMasked[p.description] {
				if !done {
					masked, done = lower, true
					if !hasCarrier(variant) {
						masked = strings.ToLower(maskQuotedProse(variant))
					}
				}
				target = masked
			}
			if matches(p.re, target) {
				return p.description
			}
		}
	}
	return ""
}

// Walks every dangerous description the command matches, in order, until each returns false; only the rules named when only is set
func (r *reader) walkDangerous(command string, only map[string]bool, each func(string) bool) {
	if tooLarge(command) {
		if only == nil {
			each(parserLimit)
		}
		return
	}
	if scratchCleanup(command) {
		return
	}
	rules := dangerous
	if only != nil {
		rules = nil
		for _, p := range dangerous {
			if only[p.description] {
				rules = append(rules, p)
			}
		}
	}
	for _, variant := range r.variants(command) {
		lower := lowerKeepingFlags(variant)
		masked, done := "", false
		for _, p := range rules {
			target := lower
			if dangerousMasked[p.description] {
				if !done {
					masked, done = lowerKeepingFlags(maskQuotedProse(variant)), true
				}
				target = masked
			}
			if matches(p.re, target) && !each(p.description) {
				return
			}
		}
	}
	if only != nil {
		if only[invisibleUnicode] && matches(hiddenSelectors, command) {
			each(invisibleUnicode)
		}
		return
	}
	for _, f := range execFindings(r.normalize(command)) {
		if !each(f.description) {
			return
		}
	}
	if containsLifecycle(command, r.profile) && !each(lifecycleSplice) {
		return
	}
	if matches(hiddenSelectors, command) {
		each(invisibleUnicode)
	}
}

// First dangerous description the command matches, empty when none
func (r *reader) dangerous(command string) string {
	found := ""
	r.walkDangerous(command, nil, func(d string) bool {
		found = d
		return false
	})
	return found
}

// Every content rule the command matches, in order
func (r *reader) contents(command string) []string {
	var keys []string
	r.walkDangerous(command, contentRules, func(d string) bool {
		if !slices.Contains(keys, d) {
			keys = append(keys, d)
		}
		return true
	})
	return keys
}

// Keys still waiting for consent and their descriptions joined, false when nothing is flagged or all are allowed
func (r *reader) unapproved(command string, approved func(key string) bool) ([]string, string, bool) {
	first := r.dangerous(command)
	if first == "" {
		return nil, "", false
	}
	var keys, texts []string
	for _, key := range append([]string{first}, slices.DeleteFunc(r.contents(command), func(k string) bool { return k == first })...) {
		if !approved(key) {
			keys = append(keys, key)
			texts = append(texts, key)
		}
	}
	if len(keys) == 0 {
		return nil, "", false
	}
	return keys, strings.Join(texts, "; "), true
}

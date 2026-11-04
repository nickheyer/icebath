package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
	"golang.org/x/text/unicode/norm"
)

const (
	sshSensitivePath   = `(?:~|\$home|\$\{home\})/\.ssh(?:/|$)`
	icebathHomePrefix  = `(?:~\/\.(?:icebath|hermes)/|(?:\$home|\$\{home\})/\.(?:icebath|hermes)/|(?:\$icebath_home|\$\{icebath_home\}|\$hermes_home|\$\{hermes_home\})/)`
	icebathEnvPath     = icebathHomePrefix + `\.env\b`
	icebathConfigPath  = icebathHomePrefix + `config\.yaml\b`
	projectEnvPath     = `(?:(?:/|\.{1,2}/)?(?:[^\s/"'` + "`" + `]+/)*\.env(?:\.[^/\s"'` + "`" + `]+)*)`
	projectConfigPath  = `(?:(?:/|\.{1,2}/)?(?:[^\s/"'` + "`" + `]+/)*config\.yaml)`
	shellRcFiles       = `(?:~|\$home|\$\{home\})/\.(?:bashrc|zshrc|profile|bash_profile|zprofile)\b`
	credentialFiles    = `(?:~|\$home|\$\{home\})/\.(?:netrc|pgpass|npmrc|pypirc)\b`
	homePrefix         = `(?:~|\$home|\$\{home\})/`
	secretFile         = `(?:` + sshSensitivePath + `|` + icebathEnvPath + `|` + credentialFiles + `|` + projectEnvPath + `(?![\w-])` + `|` + homePrefix + `\.(?:aws/credentials|git-credentials|config/gh/hosts\.yml|docker/config\.json|kube/config)\b` + `|/etc/(?:shadow|passwd)\b|\bid_(?:rsa|ed25519|ecdsa|dsa)\b)`
	secretVar          = `\$\{?\w*(?:key|token|secret|passwd|password|credential)\w*\}?`
	uploadFlag         = `(?<!\S)(?:(?-i:-[a-zA-Z]*[dFT])|--data(?:-binary|-raw|-urlencode|-ascii)?|--form(?:-string)?|--upload-file|--json|--post-(?:data|file)|--body-(?:data|file))`
	emojiRange         = `\U0001F000-\U0001FAFF☀-➿`
	emojiPresentBases  = `©®‼⁉™ℹ↔-↙↩↪⌚⌛⌨⏏⏩-⏳⏸-⏺Ⓜ▪▫▶◀◻-◾⤴⤵⬅-⬇⬛⬜⭐⭕〰〽㊗㊙` + emojiRange
	globalFlags        = `(?:-\S++(?:\s++(?!-\S)\S++)?\s++)*`
	containerGlobFlags = `(?:-\S++(?:\s(?!-\S)\S++)?\s++)*`
	macosPrivateSysPth = `/private/(?:etc|var|tmp|home)/`
	systemConfigPath   = `(?:/etc/|` + macosPrivateSysPth + `)`
	sensitiveWriteTgt  = `(?:` + systemConfigPath + `|/dev/sd|` + sshSensitivePath + `|` + icebathEnvPath + `|` + icebathConfigPath + `|` + shellRcFiles + `|` + credentialFiles + `)`
	userSensitiveWrite = `(?:` + sshSensitivePath + `|` + shellRcFiles + `|` + credentialFiles + `)`
	projectSensitiveWr = `(?:` + projectEnvPath + `|` + projectConfigPath + `)`
	commandTail        = `(?:\s*(?:&&|\|\||;).*)?$`
	writeTargetBound   = `(?=[\s;&|<>"']|$)`
	cmdPos             = `(?:^|[\n` + "`" + `]|\$\()` + `\s*` + `(?:sudo\s+(?:-[^\s]+\s+)*)?` + `(?:env\s+(?:\w+=\S*\s+)*)?` + `(?:(?:exec|nohup|setsid|time)\s+)*` + `\s*`
	hardlineSystemDirs = `/home|/home/\*|/root|/root/\*|/etc|/etc/\*|/usr|/usr/\*|/var|/var/\*|/bin|/bin/\*|/sbin|/sbin/\*|/boot|/boot/\*|/lib|/lib/\*`
	rmFlagPrefix       = cmdPos + `rm\s+(-[^\s]*\s+)*`
	pkgOpts            = `(?:-[^\s]+(?:\s+[^-\s][^\s]*)?\s+)*`
	shellNamesRe       = `bash|sh|zsh|ksh|dash`

	InvisibleUnicodeDescription       = "invisible or bidirectional Unicode control character (possible obfuscation)"
	ParserLimitDescription            = "command parser limit exceeded"
	MalformedExecDescription          = "command parser limit or malformed executable payload"
	GatewayLifecycleSpliceDescription = "stop/restart icebath gateway via shell-spliced verb (kills running agents)"

	maxDetectionCommandChars     = 128000
	maxSeparatorFreeCommandChars = 4096
	maxDetectionSegments         = 25000
)

var shellNames = []string{"bash", "sh", "zsh", "ksh", "dash"}

var shellCarrierNames = setOf("eval", "sh", "bash", "zsh", "ksh", "dash", "source", ".")

// Destructive path matcher accepting a quoted path pair or a bare path with a terminator
func hardlineRmPath(pathAlt string) string {
	return `(?:["'](?:` + pathAlt + `)["']|(?:` + pathAlt + `)(?:\s|$|[)` + "`" + `;|&]))`
}

// DangerPattern is one Python syntax regex and the description that doubles as its approval key
type DangerPattern struct {
	Pattern     string
	Description string
}

// HardlinePatterns never run through the agent regardless of yolo or approval mode
var HardlinePatterns = []DangerPattern{
	{rmFlagPrefix + hardlineRmPath(`/(?:(?:\.\.?)?/)*(?:\.\.?)?\**|/ \*`), "recursive delete of root filesystem"},
	{rmFlagPrefix + hardlineRmPath(hardlineSystemDirs), "recursive delete of system directory"},
	{rmFlagPrefix + hardlineRmPath(`(?:~|\$\{?HOME\}?)(?:/?|/\*)?`), "recursive delete of home directory"},
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

var quoteMaskedHardlineDescriptions = setOf("redirect to raw block device", "fork bomb")

// DangerousPatterns require approval before the command runs
var DangerousPatterns = []DangerPattern{
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
	{`\bappdata[\\/](?:local|roaming)[\\/](?:icebath|hermes)[^\n]*\.env\b`, "access to Icebath secrets (Windows path)"},
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
	{`>\s*` + systemConfigPath, "overwrite system config"},
	{`\bsystemctl\s+(-[^\s]+\s+)*(stop|restart|disable|mask)\b`, "stop/restart system service"},
	{`\bkill\s+-9\s+-1\b`, "kill all processes"},
	{`\bpkill\s+-9\b`, "force kill processes"},
	{`\bkillall\s+(-[^\s]*\s+)*-(9|KILL|SIGKILL)\b`, "force kill processes (killall -KILL)"},
	{`\bkillall\s+(-[^\s]*\s+)*-s\s+(KILL|SIGKILL|9)\b`, "force kill processes (killall -s KILL)"},
	{`\bkillall\s+(-[^\s]*\s+)*-r\b`, "kill processes by regex (killall -r)"},
	{`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`, "fork bomb"},
	{`\b(curl|wget)\b.*\|\s*(?:[/\w]*/)?(?:` + shellNamesRe + `)(?:\s|$|-c)`, "pipe remote content to shell"},
	{`\b(?:` + shellNamesRe + `)\s+<\s*<?\s*\(\s*(curl|wget)\b`, "execute remote script via process substitution"},
	{`(?:\beval\b|\bsource\b|\.)\s*(?:\$\(\s*|` + "`" + `\s*)(?:curl|wget)\b`, "execute remote content via command substitution"},
	{`(?<![\d.])(?:169\.254\.169\.254|100\.100\.100\.200)(?![\d.])|(?<![\w.-])metadata\.google\.internal(?![\w.-])|fd00:ec2::254`, "cloud metadata endpoint access (instance credentials)"},
	{`\b(base64|base32|base16)\s+(?:-[dD]|--decode)\b.*\|\s*\b(?:` + shellNamesRe + `)\b`, "pipe decoded content to shell (possible command obfuscation)"},
	{`\bxxd\s+-r\b.*\|\s*\b(?:` + shellNamesRe + `)\b`, "pipe xxd-decoded content to shell (possible command obfuscation)"},
	{`\becho\b[^|]*\|\s*\btr\b[^|]*\|\s*\b(?:` + shellNamesRe + `)\b`, "pipe tr-transformed output to shell (possible command obfuscation)"},
	{`\bopenssl\b.*\b(?:base64|enc)\b[^|]*\s+-[dD]\b[^|]*\|\s*\b(?:` + shellNamesRe + `)\b`, "pipe openssl-decoded content to shell (possible command obfuscation)"},
	{uploadFlag + `(?:[\s=]+|(?=[@"'$]))(?:"[^"]*(?:` + secretVar + `|` + secretFile + `)|'[^']*` + secretFile + `|\$\([^)]*` + secretFile + `|\S*?(?:` + secretVar + `|` + secretFile + `))`, "upload a secret or credential file via curl/wget (possible exfiltration)"},
	{secretFile + `[^|;&\n]*\|[^;&|\n]*\b(?:curl|wget)\b[^;&|\n]*` + uploadFlag, "pipe a credential file into a curl/wget upload (possible exfiltration)"},
	{`[\u200b\u200c\u200e\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff\u180e\U000e0000-\U000e007f]|(?<![` + emojiRange + `\ufe0f])\u200d|\u200d(?![` + emojiRange + `])`, InvisibleUnicodeDescription},
	{`\btee\b.*["']?` + sensitiveWriteTgt, "overwrite system file via tee"},
	{`>>?\s*["']?` + sensitiveWriteTgt, "overwrite system file via redirection"},
	{`\btee\b.*["']?` + projectSensitiveWr + `["']?` + writeTargetBound, "overwrite project env/config via tee"},
	{`>>?\s*["']?` + projectSensitiveWr + `["']?` + writeTargetBound, "overwrite project env/config via redirection"},
	{`\bxargs\s+.*\brm\b`, "xargs with rm"},
	{`\bfind\b.*-exec(?:dir)?\s+(/\S*/)?rm\b`, "find -exec/-execdir rm"},
	{cmdPos + `find\s[^;|&\n]*(?<!\S)-(?:\{[^}\s]*(?:delete|exec(?:dir)?)[^}\s]*\}|(?:del(?:ete?)?|exec(?:dir)?)[*?\[])`, "find dynamic shell word may expand to destructive flag"},
	{`\bfind\b.*-delete\b`, "find -delete"},
	{`\b(?:rg|sort|ag|man)\b[^;|&\n]*(?<!\S)--(?:pre|hostname-bin|compress-program|pager|html)(?:\{|[*?\[])`, "dynamic shell word may expand to arbitrary program execution flag"},
	{`\b(?:icebath|hermes)\s+` + globalFlags + `gateway\s+(stop|restart)\b`, "stop/restart icebath gateway (kills running agents)"},
	{`\b(?:icebath|hermes)\s+update\b`, "icebath update (restarts gateway, kills running agents)"},
	{`\bdocker\s+` + containerGlobFlags + `(?:-h|--host)[=\s]+\S+`, "docker with remote daemon redirect (-H/--host)"},
	{`\bdocker\s+` + containerGlobFlags + `(?:-c|--context)[=\s]+\S+`, "docker with daemon redirect (--context: alternate daemon)"},
	{`\bdocker\s+context\s+use\b`, "docker context use (switches default daemon for future commands)"},
	{`\bpodman\s+` + containerGlobFlags + `(?:--url|--connection|--identity)[=\s]+\S+`, "podman with remote daemon redirect (--url/--connection/--identity)"},
	{`\bpodman\s+` + containerGlobFlags + `(?:-r\b|--remote\b)`, "podman remote mode (-r/--remote: remote daemon)"},
	{`\b(?:docker_host|docker_context|container_host|container_connection)=\S+`, "docker/podman daemon redirect via environment (DOCKER_HOST/CONTAINER_HOST)"},
	{`\bdocker(?:-compose|\s+compose)\s+` + containerGlobFlags + `(restart|stop|kill|down)\b`, "docker compose restart/stop/kill/down (container lifecycle)"},
	{`\bdocker\s+` + containerGlobFlags + `(restart|stop|kill)\b`, "docker restart/stop/kill (container lifecycle)"},
	{`gateway\s+run\b.*(&\s*$|&\s*;|\bdisown\b|\bsetsid\b)`, "start gateway outside systemd (use 'systemctl --user restart icebath-gateway')"},
	{`\bnohup\b.*gateway\s+run\b`, "start gateway outside systemd (use 'systemctl --user restart icebath-gateway')"},
	{`\b(pkill|killall)\b.*\b(icebath|hermes|gateway|cli\.py)\b`, "kill icebath/gateway process (self-termination)"},
	{`\bkill\b.*\$\(\s*(pgrep|pidof)\b`, "kill process via pgrep/pidof expansion (self-termination)"},
	{`\bkill\b.*` + "`" + `\s*(pgrep|pidof)\b`, "kill process via backtick pgrep/pidof expansion (self-termination)"},
	{`\A(?=[\s\S]*\blaunchctl\s+(?:stop|kickstart|bootout|unload|kill|disable|remove)\b)(?=[\s\S]*\b(?:icebath|hermes|ai\.icebath|ai\.hermes)\b)`, "stop/restart icebath launchd service (kills running agents)"},
	{`\b(cp|mv|install)\b.*\s` + systemConfigPath, "copy/move file into system config path"},
	{`\b(cp|mv|install)\b.*\s["']?` + projectSensitiveWr + `["']?` + commandTail, "overwrite project env/config file"},
	{`\b(cp|mv|install)\b.*\s["']?` + sensitiveWriteTgt + `[^\s"']*["']?` + commandTail, "copy/move file into sensitive credential/SSH/shell-rc path"},
	{`\bsed\s+-[^\s]*i.*(?:` + userSensitiveWrite + `)[^\s"']*`, "in-place edit of sensitive credential/SSH/shell-rc path"},
	{`\bsed\s+--in-place\b.*(?:` + userSensitiveWrite + `)[^\s"']*`, "in-place edit of sensitive credential/SSH/shell-rc path (long flag)"},
	{`\b(?:perl|ruby)\b.*(?:^|\s)-[^\s]*i\b.*(?:` + userSensitiveWrite + `)[^\s"']*`, "in-place edit of sensitive credential/SSH/shell-rc path (perl/ruby)"},
	{`\bsed\s+-[^\s]*i.*\s` + systemConfigPath, "in-place edit of system config"},
	{`\bsed\s+--in-place\b.*\s` + systemConfigPath, "in-place edit of system config (long flag)"},
	{`\bsed\s+-[^\s]*i.*(?:` + icebathConfigPath + `|` + icebathEnvPath + `)`, "in-place edit of Icebath config/env"},
	{`\bsed\s+--in-place\b.*(?:` + icebathConfigPath + `|` + icebathEnvPath + `)`, "in-place edit of Icebath config/env (long flag)"},
	{`\b(?:perl|ruby)\b.*(?:^|\s)-[^\s]*i\b.*(?:` + icebathConfigPath + `|` + icebathEnvPath + `)`, "in-place edit of Icebath config/env (perl/ruby)"},
	{`\b(?:` + shellNamesRe + `)\s+<<`, "shell execution via heredoc"},
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

var quoteMaskedDangerousDescriptions = setOf(
	"find dynamic shell word may expand to destructive flag",
	"dynamic shell word may expand to arbitrary program execution flag",
)

// ContentRuleDescriptions describe what a command carries rather than which verb runs it
var ContentRuleDescriptions = setOf(
	"upload a secret or credential file via curl/wget (possible exfiltration)",
	"pipe a credential file into a curl/wget upload (possible exfiltration)",
	InvisibleUnicodeDescription,
)

var removedPatternKeyAliases = [][2]string{
	{"script execution via -e/-c flag", `(python[23]?|perl|ruby|node)\s+-[ec]\s+`},
	{"script execution via heredoc", `(python[23]?|perl|ruby|node)\s+<<`},
}

const oldBranchKey = `git\s+branch\s+-D`

type compiledPattern struct {
	re          *regexp2.Regexp
	description string
	quoteMasked bool
}

var (
	hardlineCompiled  []compiledPattern
	dangerousCompiled []compiledPattern
	patternKeyAliases = map[string]map[string]bool{}
)

var unicodeEscapeRe = regexp.MustCompile(`\\U[0-9a-fA-F]{8}|\\u[0-9a-fA-F]{4}`)

// Rewrites Python regex syntax into the dialect regexp2 compiles
func pyPatternToRegexp2(src string) string {
	s := strings.ReplaceAll(src, `\S++`, `(?>\S+)`)
	s = strings.ReplaceAll(s, `\s++`, `(?>\s+)`)
	s = strings.ReplaceAll(s, `(?P<`, `(?<`)
	return unicodeEscapeRe.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

// Compiles a Python syntax pattern with the IGNORECASE and DOTALL flags
func pyRegexp2(src string) *regexp2.Regexp {
	return regexp2.MustCompile(pyPatternToRegexp2(src), regexp2.IgnoreCase|regexp2.Singleline)
}

// Legacy approval key derived from the pattern text
func legacyPatternKey(p string) string {
	if strings.Contains(p, `\b`) {
		return strings.Split(p, `\b`)[1]
	}
	r := []rune(p)
	if len(r) > 20 {
		r = r[:20]
	}
	return string(r)
}

func addKeyAlias(a, b string) {
	if patternKeyAliases[a] == nil {
		patternKeyAliases[a] = map[string]bool{}
	}
	patternKeyAliases[a][a] = true
	patternKeyAliases[a][b] = true
}

func init() {
	for _, p := range HardlinePatterns {
		hardlineCompiled = append(hardlineCompiled, compiledPattern{pyRegexp2(p.Pattern), p.Description, quoteMaskedHardlineDescriptions[p.Description]})
	}
	for _, p := range DangerousPatterns {
		dangerousCompiled = append(dangerousCompiled, compiledPattern{pyRegexp2(p.Pattern), p.Description, false})
		legacy := legacyPatternKey(p.Pattern)
		addKeyAlias(p.Description, legacy)
		addKeyAlias(legacy, p.Description)
	}
	for _, pair := range removedPatternKeyAliases {
		addKeyAlias(pair[0], pair[1])
		addKeyAlias(pair[1], pair[0])
	}
	addKeyAlias("git branch force delete", oldBranchKey)
	addKeyAlias(oldBranchKey, "git branch force delete")
}

// ApprovalKeyAliases lists every approval key a pattern may be stored under
func ApprovalKeyAliases(patternKey string) map[string]bool {
	if set, ok := patternKeyAliases[patternKey]; ok {
		out := make(map[string]bool, len(set))
		for k := range set {
			out[k] = true
		}
		return out
	}
	return map[string]bool{patternKey: true}
}

var hiddenVariationSelectorRe = regexp2.MustCompile(pyPatternToRegexp2(`(?<![` + emojiPresentBases + `])(?!(?<=[0-9#*])️⃣)[︀-️\U000e0100-\U000e01ef]`))

func regexp2Match(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// Python os.path.basename, everything after the last slash
func pyBasename(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// Reports whether a rune is whitespace the way str.isspace does
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func runeAt(s string, i int) (rune, int) {
	r, size := utf8.DecodeRuneInString(s[i:])
	if size == 0 {
		return 0, 1
	}
	return r, size
}

func prevRuneAt(s string, i int) rune {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func partition(s, sep string) (string, bool, string) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], true, s[i+len(sep):]
	}
	return s, false, ""
}

var (
	backslashNewlineRe = regexp.MustCompile(`\\\r?\n`)
	escapeStripRe      = regexp.MustCompile(`\\([^\n])`)
	ifsRe              = regexp.MustCompile(`\$\{IFS\b[^}]*\}|\$IFS\b`)
	emptyLiteralRepl   = strings.NewReplacer("''", "", `""`, "")
)

// Normalizes a command so escapes, fullwidth forms, home spellings, and splicing cannot bypass detection
func normalizeCommandForDetection(command string) string {
	command = norm.NFKC.String(strings.ReplaceAll(StripANSI(command), "\x00", ""))
	command = backslashNewlineRe.ReplaceAllString(command, "")
	command = rewriteResolvedIcebathHome(command)
	command = rewriteResolvedUserHome(command)
	command = escapeStripRe.ReplaceAllString(command, "${1}")
	command = emptyLiteralRepl.Replace(command)
	return ifsRe.ReplaceAllString(command, " ")
}

// Lowercases a variant while keeping dash prefixed tokens byte for byte
func lowerPreservingFlags(command string) string {
	var b strings.Builder
	i := 0
	for i < len(command) {
		r, size := runeAt(command, i)
		if pyIsSpace(r) {
			j := i
			for j < len(command) {
				r2, s2 := runeAt(command, j)
				if !pyIsSpace(r2) {
					break
				}
				j += s2
			}
			b.WriteString(command[i:j])
			i = j
			continue
		}
		j := i + size
		for j < len(command) {
			r2, s2 := runeAt(command, j)
			if pyIsSpace(r2) {
				break
			}
			j += s2
		}
		tok := command[i:j]
		if strings.HasPrefix(tok, "-") {
			b.WriteString(tok)
		} else {
			b.WriteString(strings.ToLower(tok))
		}
		i = j
	}
	return b.String()
}

const pathTokenStop = `\s'"` + "`" + `;|&<>()`
const pathTail = `(?P<tail>(?:[/\\][^/\\` + pathTokenStop + `]*)+)`

var (
	homeFoldMu    sync.Mutex
	homeFoldCache = map[string]*regexp.Regexp{}
	pathSplitRe   = regexp.MustCompile(`[/\\]+`)
)

// Regex matching a resolved home as an absolute directory prefix, nil for a degenerate path
func homePrefixFoldRegex(path string) *regexp.Regexp {
	homeFoldMu.Lock()
	defer homeFoldMu.Unlock()
	if re, ok := homeFoldCache[path]; ok {
		return re
	}
	var components []string
	if path != "" {
		for _, c := range pathSplitRe.Split(path, -1) {
			if c != "" {
				components = append(components, c)
			}
		}
	}
	var re *regexp.Regexp
	if len(components) >= 2 {
		quoted := make([]string, len(components))
		for i, c := range components {
			quoted[i] = regexp.QuoteMeta(c)
		}
		re = regexp.MustCompile(`[/\\]*` + strings.Join(quoted, `[/\\]+`) + pathTail)
	}
	if len(homeFoldCache) >= 64 {
		homeFoldCache = map[string]*regexp.Regexp{}
	}
	homeFoldCache[path] = re
	return re
}

// Folds each resolved home prefix to the replacement, longest first
func foldHomePrefixes(command string, paths []string, replacement string) string {
	var uniq []string
	seen := map[string]bool{}
	sorted := append([]string(nil), paths...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, p := range sorted {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	for _, p := range uniq {
		re := homePrefixFoldRegex(p)
		if re == nil {
			continue
		}
		tailIdx := re.SubexpIndex("tail")
		command = re.ReplaceAllStringFunc(command, func(m string) string {
			sub := re.FindStringSubmatch(m)
			return replacement + strings.ReplaceAll(sub[tailIdx], "\\", "/")
		})
	}
	return command
}

// Folds the user home spelled absolutely to a tilde
func rewriteResolvedUserHome(command string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return command
	}
	real := h
	if r, err := filepath.EvalSymlinks(h); err == nil {
		real = r
	}
	return foldHomePrefixes(command, []string{h, real, os.Getenv("HOME")}, "~")
}

// Folds the resolved icebath home to the tilde spelling the config and env rules match
func rewriteResolvedIcebathHome(command string) string {
	h := expandTilde(home.Process())
	if h == "" {
		return command
	}
	return foldHomePrefixes(command, []string{h, resolveNonStrict(h)}, "~/.icebath")
}

var (
	paramReplacementRe   = regexp.MustCompile(`\$\{[^}/\s]+/[^}/]*/(?P<replacement>[^}]*)\}`)
	paramDefaultRe       = regexp.MustCompile(`\$\{[^}:}\s]+:-(?P<default>[^}]*)\}`)
	simpleShellLiteralRe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)
	envAssignmentFullRe  = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*=[^\n]*\z`)
	echoFlagRe           = regexp.MustCompile(`\A-[nEe]+\z`)
)

var commandWrapperWords = setOf("sudo", "env", "exec", "nohup", "setsid", "time", "command", "builtin",
	"nice", "timeout", "stdbuf", "ionice", "chrt", "taskset", "chroot")

var sudoOptionsWithArg = setOf("-c", "--close-from", "-g", "--group", "-h", "--host", "-p", "--prompt", "-u", "--user")

var commandWrapperOptionsWithArg = map[string]map[string]bool{
	"chroot":  setOf("--groups", "--userspec"),
	"sudo":    sudoOptionsWithArg,
	"env":     setOf("-a", "--argv0", "-C", "--chdir", "-S", "--split-string", "-u", "--unset"),
	"exec":    setOf("-a"),
	"nice":    setOf("-n", "--adjustment"),
	"time":    setOf("-f", "--format", "-o", "--output"),
	"timeout": setOf("-k", "--kill-after", "-s", "--signal"),
	"stdbuf":  setOf("-e", "--error", "-i", "--input", "-o", "--output"),
	"ionice":  setOf("-c", "--class", "-n", "--classdata"),
}

var commandWrapperNonExecutingOptions = map[string]map[string]bool{
	"command": setOf("-v", "-V"),
	"chrt":    setOf("-p", "--pid"),
	"ionice":  setOf("-p", "--pid", "--pgid", "--uid"),
	"taskset": setOf("-p", "--pid"),
}

var commandWrapperPositionalArgs = map[string]int{"chroot": 1, "chrt": 1, "taskset": 1, "timeout": 1}

var shellCommandTransitions = setOf("if", "then", "else", "elif", "do", "while", "until", "!")

type interpreterNameRe struct {
	family string
	re     *regexp.Regexp
}

var interpreterNameRes = []interpreterNameRe{
	{"python", regexp.MustCompile(`\A(?:py(?:\.exe)?|python[23]?(?:\.\d+)*(?:\.exe)?)\z`)},
	{"node", regexp.MustCompile(`\A(?:node(?:js)?(?:\.exe)?)\z`)},
	{"perl", regexp.MustCompile(`\A(?:perl[0-9]*(?:\.\d+)*(?:\.exe)?)\z`)},
	{"ruby", regexp.MustCompile(`\A(?:ruby[0-9.]*(?:\.exe)?)\z`)},
	{"php", regexp.MustCompile(`\A(?:php(?:\.exe)?)\z`)},
	{"powershell", regexp.MustCompile(`\A(?:powershell(?:\.exe)?|pwsh(?:\.exe)?)\z`)},
	{"bun", regexp.MustCompile(`\A(?:bun(?:\.exe)?)\z`)},
	{"deno", regexp.MustCompile(`\A(?:deno(?:\.exe)?)\z`)},
}

var interpreterExecFlags = map[string]map[string]bool{
	"python": setOf("-c"), "node": setOf("-e", "--eval", "-p", "--print"), "perl": setOf("-e", "--eval"), "ruby": setOf("-e"),
	"php": setOf("-r"), "powershell": setOf("-command", "-c", "-file", "-f"),
	"bun": setOf("-e", "--eval"), "deno": setOf("eval", "-e", "--eval"),
}

var interpreterWithArg = map[string]map[string]bool{
	"python": setOf("-W", "-X", "--check-hash-based-pycs"),
	"node": setOf("-C", "--conditions", "--cpu-prof-dir", "--diagnostic-dir", "--icu-data-dir", "--import", "--loader",
		"--openssl-config", "--require", "--title"),
	"perl": setOf("-0", "-F", "-I", "-M", "-m", "-x"), "ruby": setOf("-C", "-E", "-F", "-I", "-K", "-r"),
	"php": setOf("-c", "-d", "-z"),
	"powershell": setOf("-configurationname", "-custompipename", "-executionpolicy", "-inputformat", "-outputformat",
		"-settingsfile", "-version", "-windowstyle", "-workingdirectory"),
	"bun": setOf("--config", "--cwd", "--env-file", "--preload", "--require"), "deno": setOf("-L", "--log-level"),
}

var readToolExecFlags = map[string]map[string]bool{
	"sort": setOf("--compress-program"), "rg": setOf("--pre", "--hostname-bin"), "ag": setOf("--pager"),
	"man": setOf("--pager", "--html", "-P", "-H"),
}

var readToolLongOptionsWithArg = map[string]map[string]bool{
	"rg": setOf(
		"--after-context", "--before-context", "--color", "--colors", "--context", "--context-separator",
		"--dfa-size-limit", "--encoding", "--engine", "--field-context-separator", "--field-match-separator",
		"--file", "--generate", "--glob", "--hostname-bin", "--hyperlink-format", "--iglob", "--ignore-file",
		"--max-columns", "--max-count", "--max-depth", "--max-filesize", "--path-separator", "--pre", "--pre-glob",
		"--regex-size-limit", "--regexp", "--replace", "--sort", "--sortr", "--threads", "--type", "--type-add",
		"--type-clear", "--type-not",
	),
	"sort": setOf(
		"--batch-size", "--buffer-size", "--compress-program", "--field-separator", "--files0-from", "--key",
		"--output", "--parallel", "--random-source", "--sort", "--temporary-directory",
	),
	"man": setOf(
		"--config-file", "--encoding", "--extension", "--locale", "--manpath", "--pager", "--preprocessor",
		"--prompt", "--recode", "--sections", "--systems",
	),
	"ag": setOf(
		"--ackmate-dir-filter", "--color-line-number", "--color-match", "--color-path", "--depth",
		"--filename-pattern", "--file-search-regex", "--ignore", "--ignore-dir", "--max-count", "--pager",
		"--path-to-ignore", "--width", "--workers",
	),
}

var readToolShortOptionsWithArg = map[string]string{
	"rg": "efEmjgdtTABCMr", "sort": "koStT", "man": "CRLmMSserEPp", "ag": "gGmpW",
}

var grepOptionsWithArg = setOf(
	"--after-context", "--before-context", "--binary-files", "--context", "--directories", "--devices", "--exclude",
	"--exclude-dir", "--exclude-from", "--include", "--label", "--max-count", "--regexp", "--file",
)

const grepShortOptionsWithArg = "ABCDdefm"

var bashOptionsWithArg = setOf("-O", "+O", "-o", "+o", "--init-file", "--rcfile")

const bashShortOptionLetters = "ilrsDcabefhkmnptuvxBCEHPTOo"

// Reports whether the command is too large or too segmented to inspect safely
func commandParserLimitExceeded(command string) bool {
	if utf8.RuneCountInString(command) > maxDetectionCommandChars {
		return true
	}
	if utf8.RuneCountInString(command) > maxSeparatorFreeCommandChars && !strings.ContainsAny(command, ";&|\n") {
		return true
	}
	total := 0
	for _, c := range []string{";", "&", "|", "\n"} {
		total += strings.Count(command, c)
	}
	return total >= maxDetectionSegments
}

// Index of the backtick closing the one opened at i, or -1
func backtickEndFrom(segment string, i int) int {
	j := strings.Index(segment[i+1:], "`")
	if j < 0 {
		return -1
	}
	j += i + 1
	for j != -1 && segment[j-1] == '\\' {
		k := strings.Index(segment[j+1:], "`")
		if k < 0 {
			return -1
		}
		j = k + j + 1
	}
	return j
}

const (
	stepChar = iota
	stepEsc
	stepQuote
	stepSubst
	stepComment
)

type scanStep struct {
	kind  int
	i, j  int
	quote byte
}

type scanOpts struct {
	subst            string
	brace            bool
	stopUnterminated bool
	naiveBacktick    bool
	comments         bool
}

// Lazy lexical scanner over text without expanding, the single quote and escape state machine behind every scanner
type shellScan struct {
	text  string
	n, i  int
	quote byte
	o     scanOpts
	done  bool
}

func newShellScan(text string, start, end int, o scanOpts) *shellScan {
	n := end
	if n < 0 {
		n = len(text)
	}
	return &shellScan{text: text, n: n, i: start, o: o}
}

// Next lexical step, ok false once the scan is exhausted
func (s *shellScan) next() (scanStep, bool) {
	if s.done || s.i >= s.n {
		return scanStep{}, false
	}
	text, o, i := s.text, s.o, s.i
	ch, size := runeAt(text, i)
	kind, j := stepChar, i+size
	substWanted := "u"
	if s.quote != 0 {
		substWanted = "q"
	}
	switch {
	case o.comments && s.quote == 0 && isShellCommentStart(text, i):
		kind = stepComment
		if k := strings.Index(text[i:s.n], "\n"); k >= 0 {
			j = i + k
		} else {
			j = s.n
		}
	case s.quote != '\'' && ch == '\\' && i+1 < s.n:
		_, s2 := runeAt(text, i+1)
		kind, j = stepEsc, i+1+s2
	case (s.quote != 0 && ch == rune(s.quote)) || (s.quote == 0 && (ch == '\'' || ch == '"')):
		kind = stepQuote
	case s.quote != '\'' && strings.Contains(o.subst, substWanted) && (ch == '`' || strings.HasPrefix(text[i:], "$(") || (o.brace && s.quote == 0 && strings.HasPrefix(text[i:], "${"))):
		close := -1
		if ch == '`' {
			if o.naiveBacktick {
				if k := strings.Index(text[i+1:], "`"); k >= 0 {
					close = i + 1 + k + 1
				}
			} else {
				close = scanBacktickEnd(text, i)
			}
		} else if text[i+1] == '(' {
			close = scanDollarParenEnd(text, i)
		} else if k := strings.Index(text[i+2:], "}"); k >= 0 {
			close = i + 2 + k + 1
		}
		if close >= 0 {
			kind, j = stepSubst, close
		} else if o.stopUnterminated {
			s.done = true
			return scanStep{stepSubst, i, -1, s.quote}, true
		}
	}
	step := scanStep{kind, i, j, s.quote}
	if kind == stepQuote {
		if s.quote != 0 {
			s.quote = 0
		} else {
			s.quote = byte(ch)
		}
	}
	s.i = j
	return step, true
}

// Offset after a balanced dollar paren substitution, or -1
func scanDollarParenEnd(command string, start int) int {
	depth := 1
	for sc := newShellScan(command, start+2, -1, scanOpts{}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepChar && st.quote == 0 {
			if strings.HasPrefix(command[st.i:], "$(") {
				depth++
			}
			if command[st.i] == ')' {
				depth--
			}
			if depth == 0 {
				return st.i + 1
			}
		}
	}
	return -1
}

// Offset after the backtick closing the one at start, only a backslash escapes inside
func scanBacktickEnd(command string, start int) int {
	i := start + 1
	for i < len(command) {
		c := command[i]
		if c == '\\' {
			if i+1 >= len(command) {
				return -1
			}
			_, s2 := runeAt(command, i+1)
			i += 1 + s2
			continue
		}
		if c == '`' {
			return i + 1
		}
		_, size := runeAt(command, i)
		i += size
	}
	return -1
}

func skipShellWhitespace(command string, pos int) int {
	for pos < len(command) {
		r, size := runeAt(command, pos)
		if !pyIsSpace(r) {
			break
		}
		pos += size
	}
	return pos
}

// Reads one shell word without executing expansions
func readShellWord(command string, pos int) (int, int, string) {
	start := skipShellWhitespace(command, pos)
	end := start
	for sc := newShellScan(command, start, -1, scanOpts{subst: "u", brace: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepChar && st.quote == 0 {
			r, _ := runeAt(command, st.i)
			if pyIsSpace(r) || strings.ContainsRune(";&|<>()", r) {
				break
			}
		}
		end = st.j
	}
	return start, end, command[start:end]
}

// Resolves tiny literal echo or printf substitutions without executing a shell
func literalCommandSubstitutionOutput(script string) (string, bool) {
	tokens, err := shlexSplit(script)
	if err != nil {
		tokens = nil
	}
	if len(tokens) == 0 {
		return "", false
	}
	command, args := strings.ToLower(tokens[0]), tokens[1:]
	if command == "echo" {
		for len(args) > 0 && echoFlagRe.MatchString(args[0]) {
			args = args[1:]
		}
	} else if command != "printf" {
		return "", false
	}
	if len(args) == 1 && simpleShellLiteralRe.MatchString(args[0]) {
		return args[0], true
	}
	if command == "printf" && len(args) == 2 && args[0] == "%s" && simpleShellLiteralRe.MatchString(args[1]) {
		return args[1], true
	}
	return "", false
}

func replaceSimpleCommandSubstitutions(word string) string {
	var b strings.Builder
	i := 0
	for i < len(word) {
		opener := 0
		if strings.HasPrefix(word[i:], "$(") {
			opener = 2
		} else if word[i] == '`' {
			opener = 1
		}
		end := -1
		if opener == 2 {
			end = scanDollarParenEnd(word, i)
		} else if opener == 1 {
			end = scanBacktickEnd(word, i)
		}
		replacement, ok := "", false
		if end >= 0 {
			replacement, ok = literalCommandSubstitutionOutput(word[i+opener : end-1])
		}
		if !ok {
			_, size := runeAt(word, i)
			replacement, end = word[i:i+size], i+size
		}
		b.WriteString(replacement)
		i = end
	}
	return b.String()
}

func replaceSimpleShellExpansions(word string) string {
	word = replaceSimpleCommandSubstitutions(word)
	word = paramReplacementRe.ReplaceAllString(word, "${replacement}")
	return paramDefaultRe.ReplaceAllString(word, "${default}")
}

func stripShellWordSyntax(word string) string {
	var b strings.Builder
	for sc := newShellScan(word, 0, -1, scanOpts{}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		switch st.kind {
		case stepEsc:
			b.WriteString(word[st.i+1 : st.j])
		case stepQuote:
		default:
			b.WriteString(word[st.i:st.j])
		}
	}
	return b.String()
}

// Collapses quoting, escaping, and simple literal substitutions inside one command word
func deobfuscateShellWordForDetection(word string) string {
	for range 2 {
		previous := word
		word = stripShellWordSyntax(replaceSimpleShellExpansions(word))
		if word == previous {
			break
		}
	}
	return word
}

func isShellCommentStart(command string, index int) bool {
	if command[index] != '#' {
		return false
	}
	if index == 0 {
		return true
	}
	prev := prevRuneAt(command, index)
	return pyIsSpace(prev) || strings.ContainsRune(";&|()<>", prev)
}

// Offsets of every quote aware command start including subshell and group bodies
func iterShellCommandStarts(command string) []int {
	starts := []int{0}
	var scan func(start, end int)
	scan = func(start, end int) {
		skip := -1
		for sc := newShellScan(command, start, end, scanOpts{subst: "uq", stopUnterminated: true, comments: true}); ; {
			st, ok := sc.next()
			if !ok {
				break
			}
			if st.kind == stepSubst {
				inner := st.i + 2
				if command[st.i] == '`' {
					inner = st.i + 1
				}
				starts = append(starts, inner)
				innerEnd := end
				if st.j >= 0 {
					innerEnd = st.j - 1
				}
				scan(inner, innerEnd)
			} else if st.kind == stepChar && st.quote == 0 && st.i != skip {
				c := command[st.i]
				if strings.IndexByte("(;\n", c) >= 0 || (c == '{' && (st.i == 0 || pyIsSpace(prevRuneAt(command, st.i)) || strings.ContainsRune("(;&|)", prevRuneAt(command, st.i)))) {
					starts = append(starts, st.i+1)
				} else if c == '&' || c == '|' {
					repeated := st.i+1 < end && command[st.i+1] == c
					next := st.i + 1
					if repeated {
						skip = st.i + 1
						next++
					}
					starts = append(starts, next)
				}
			}
		}
	}
	scan(0, len(command))
	seen := map[int]bool{}
	var out []int
	for idx := 0; idx < len(starts); idx++ {
		start := skipShellWhitespace(command, starts[idx])
		if start >= len(command) || seen[start] || isShellCommentStart(command, start) {
			continue
		}
		seen[start] = true
		out = append(out, start)
		_, end, word := readShellWord(command, start)
		if shellCommandTransitions[word] {
			starts = append(starts, end)
		}
	}
	return out
}

type spliceEdit struct {
	start, end int
	text       string
}

// Applies sorted non overlapping edits in one pass
func splice(command string, edits []spliceEdit) string {
	var b strings.Builder
	previous := 0
	for _, e := range edits {
		b.WriteString(command[previous:e.start])
		b.WriteString(e.text)
		previous = e.end
	}
	b.WriteString(command[previous:])
	return b.String()
}

// Inserts a marker before each real command start so anchored patterns see group openers
func markCommandStarts(command, marker string) string {
	var offsets []int
	for _, o := range iterShellCommandStarts(command) {
		if o > 0 {
			offsets = append(offsets, o)
		}
	}
	if len(offsets) == 0 {
		return command
	}
	sort.Ints(offsets)
	edits := make([]spliceEdit, len(offsets))
	for i, o := range offsets {
		edits[i] = spliceEdit{o, o, marker}
	}
	return splice(command, edits)
}

// Replaces raw newlines inside quotes with a space, detection only
func maskQuotedNewlines(command string) string {
	if !strings.Contains(command, "\n") {
		return command
	}
	return maskQuotedNewlinesSpan(command, 0, len(command))
}

func maskQuotedNewlinesSpan(command string, start, end int) string {
	var b strings.Builder
	for sc := newShellScan(command, start, end, scanOpts{subst: "q"}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepSubst {
			bodyStart := st.i + 1
			if strings.HasPrefix(command[st.i:], "$(") {
				bodyStart = st.i + 2
			}
			bodyEnd := st.j - 1
			b.WriteString(command[st.i:bodyStart])
			b.WriteString(maskQuotedNewlinesSpan(command, bodyStart, bodyEnd))
			b.WriteString(command[bodyEnd:st.j])
		} else if st.quote != 0 && st.kind == stepChar && command[st.i] == '\n' {
			b.WriteByte(' ')
		} else {
			b.WriteString(command[st.i:st.j])
		}
	}
	return b.String()
}

// End offset of a shell redirection operator anchored at pos, or -1
func matchShellRedirection(command string, pos int) int {
	k := pos
	for k < len(command) && command[k] >= '0' && command[k] <= '9' {
		k++
	}
	rest := command[k:]
	for _, op := range []string{">>", "<<", "<>", ">&", "<&", ">|"} {
		if strings.HasPrefix(rest, op) {
			return k + 2
		}
	}
	if strings.HasPrefix(rest, "<") || strings.HasPrefix(rest, ">") {
		return k + 1
	}
	return -1
}

type wordSpan struct {
	start, end int
	word       string
}

// Command position words that may be executable names
func iterShellCommandWordSpans(command string) []wordSpan {
	var out []wordSpan
	for _, pos := range iterShellCommandStarts(command) {
		wrapper, positionals := "", 0
		options, skipArg := true, false
		for pos < len(command) {
			if redirect := matchShellRedirection(command, skipShellWhitespace(command, pos)); redirect >= 0 {
				_, pos, _ = readShellWord(command, redirect)
				continue
			}
			wordStart, wordEnd, word := readShellWord(command, pos)
			if wordStart == wordEnd {
				break
			}
			pos = wordEnd
			deobfuscated := deobfuscateShellWordForDetection(word)
			name := strings.ToLower(pyBasename(deobfuscated))
			if skipArg {
				skipArg = false
				continue
			}
			if wrapper != "" && options && deobfuscated == "--" {
				options = false
				continue
			}
			if wrapper != "" && options && strings.HasPrefix(deobfuscated, "-") {
				option, _, _ := partition(deobfuscated, "=")
				if wrapper == "env" && (option == "--split-string" || strings.HasPrefix(deobfuscated, "-S")) {
					break
				}
				queries := commandWrapperNonExecutingOptions[wrapper]
				if queries[option] || (wrapper == "command" && !strings.HasPrefix(option, "--") && strings.ContainsAny(option[1:], "vV")) {
					break
				}
				skipArg = !strings.Contains(deobfuscated, "=") && commandWrapperOptionsWithArg[wrapper][option]
				continue
			}
			if positionals > 0 {
				positionals--
				continue
			}
			if envAssignmentFullRe.MatchString(word) {
				continue
			}
			out = append(out, wordSpan{wordStart, wordEnd, word})
			if !commandWrapperWords[name] {
				break
			}
			wrapper, options = name, true
			positionals = commandWrapperPositionalArgs[name]
		}
	}
	return out
}

// Bounds a candidate to its command, preserving quoted argument bytes
func shellCommandSegment(command string, start int) string {
	end := len(command)
	for sc := newShellScan(command, start, -1, scanOpts{subst: "uq", brace: true, comments: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepComment || (st.kind == stepChar && st.quote == 0 && strings.IndexByte(";&|\n)`", command[st.i]) >= 0) {
			end = st.i
			break
		}
	}
	return strings.TrimSpace(command[start:end])
}

type spanToken struct {
	value      string
	start, end int
	quoted     bool
}

// Shell words with source spans for one simple command, ok false on malformed quoting
func shellTokensWithSpans(segment string, start int) ([]spanToken, bool) {
	var tokens []spanToken
	var value strings.Builder
	tokenStart := -1
	var quote byte
	depth := 0
	inBacktick := false
	flush := func(end int) {
		raw := segment[tokenStart:end]
		inert := (strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'")) || (strings.Contains(raw, "='") && strings.HasSuffix(raw, "'"))
		tokens = append(tokens, spanToken{value.String(), tokenStart, end, inert})
	}
	endAt := len(segment)
	for sc := newShellScan(segment, start, -1, scanOpts{}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		ch, _ := runeAt(segment, st.i)
		if st.kind == stepChar && quote == 0 {
			if pyIsSpace(ch) && ch != '\n' {
				if tokenStart >= 0 {
					flush(st.i)
					value.Reset()
					tokenStart = -1
				}
				continue
			}
			stop := false
			switch {
			case strings.HasPrefix(segment[st.i:], "$("):
				depth++
			case ch == '`':
				if inBacktick {
					inBacktick = false
				} else if depth == 0 && backtickEndFrom(segment, st.i) < 0 {
					endAt = st.i
					stop = true
				} else {
					inBacktick = true
				}
			case ch == ')':
				if depth == 0 {
					endAt = st.i
					stop = true
				} else {
					depth--
				}
			case strings.ContainsRune(";|&\n", ch):
				endAt = st.i
				stop = true
			}
			if stop {
				break
			}
		}
		if tokenStart < 0 {
			tokenStart = st.i
		}
		switch {
		case st.kind == stepQuote:
			if quote != 0 {
				quote = 0
			} else {
				quote = byte(ch)
			}
		case st.kind == stepEsc:
			value.WriteString(segment[st.i+1 : st.j])
		case ch == '\\' && quote == 0:
			return nil, false
		default:
			value.WriteString(segment[st.i:st.j])
		}
	}
	if quote != 0 {
		return nil, false
	}
	if tokenStart >= 0 {
		flush(endAt)
	}
	return tokens, true
}

// Spans of quoted grep PCRE operands, malformed true when the parse is ambiguous
func quotedGrepPatternSpans(command string) ([][2]int, bool) {
	var spans [][2]int
	offset := 0
	for _, segment := range iterTopLevelShellSegments(command) {
		segmentAt := offset + strings.Index(command[offset:], segment)
		offset = segmentAt + len(segment)
		for _, ws := range iterShellCommandWordSpans(segment) {
			name := strings.ToLower(pyBasename(deobfuscateShellWordForDetection(ws.word)))
			if name != "grep" && name != "egrep" {
				continue
			}
			tokens, ok := shellTokensWithSpans(segment, ws.start)
			if !ok {
				return nil, true
			}
			var args []spanToken
			if len(tokens) > 0 {
				args = tokens[1:]
			}
			var patternIndexes []int
			pcre, explicitPatterns := false, false
			operandIndex, i, options := -1, 0, true
			for i < len(args) {
				token := args[i].value
				switch {
				case options && token == "--":
					options = false
				case options && strings.HasPrefix(token, "--"):
					option, equals, _ := partition(token, "=")
					pcre = pcre || option == "--perl-regexp"
					explicitPatterns = explicitPatterns || option == "--regexp" || option == "--file"
					takesNext := grepOptionsWithArg[option] && !equals
					if takesNext && i+1 >= len(args) {
						return nil, true
					}
					if option == "--regexp" {
						if takesNext {
							patternIndexes = append(patternIndexes, i+1)
						} else {
							patternIndexes = append(patternIndexes, i)
						}
					}
					if takesNext {
						i++
					}
				case options && strings.HasPrefix(token, "-") && token != "-":
					chars := []rune(token[1:])
					for j, char := range chars {
						pcre = pcre || char == 'P'
						explicitPatterns = explicitPatterns || char == 'e' || char == 'f'
						if strings.ContainsRune(grepShortOptionsWithArg, char) {
							attached := j+1 < len(chars)
							if !attached && i+1 >= len(args) {
								return nil, true
							}
							if char == 'e' {
								if attached {
									patternIndexes = append(patternIndexes, i)
								} else {
									patternIndexes = append(patternIndexes, i+1)
								}
							}
							if !attached {
								i++
							}
							break
						}
					}
				case operandIndex < 0:
					operandIndex = i
				}
				i++
			}
			if !explicitPatterns {
				if operandIndex < 0 {
					return nil, pcre
				}
				patternIndexes = append(patternIndexes, operandIndex)
			}
			if pcre {
				for _, idx := range patternIndexes {
					t := args[idx]
					if t.quoted {
						spans = append(spans, [2]int{segmentAt + t.start, segmentAt + t.end})
					}
				}
			}
		}
	}
	return spans, false
}

// Command with quoted grep PCRE operands blanked, malformed true on an uncertain parse
func grepSafeDetectionVariant(command string) (string, bool) {
	spans, malformed := quotedGrepPatternSpans(command)
	if malformed || len(spans) == 0 {
		return command, malformed
	}
	edits := make([]spliceEdit, len(spans))
	for i, sp := range spans {
		edits[i] = spliceEdit{sp[0], sp[1], strings.Repeat(" ", utf8.RuneCountInString(command[sp[0]:sp[1]]))}
	}
	return splice(command, edits), false
}

func interpreterFamily(executable string) string {
	name := strings.ToLower(pyBasename(executable))
	for _, entry := range interpreterNameRes {
		if entry.re.MatchString(name) {
			return entry.family
		}
	}
	return ""
}

// Tokenizes an already bounded command segment, ok false on malformed quoting
func shellSegmentTokens(segment string, start int) ([]string, bool) {
	tokens, err := shlexSplitPunct(segment[start:], "<>", false)
	if err != nil {
		return nil, false
	}
	return tokens, true
}

// Top level command segments in one left to right pass
func iterTopLevelShellSegments(command string) []string {
	var out []string
	start := 0
	for sc := newShellScan(command, 0, -1, scanOpts{comments: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepComment || (st.kind == stepChar && st.quote == 0 && strings.IndexByte(";&|\n", command[st.i]) >= 0) {
			if start < st.i {
				out = append(out, command[start:st.i])
			}
			start = st.j
		}
	}
	if start < len(command) {
		out = append(out, command[start:])
	}
	return out
}

// Execution bearing interpreter option when present
func interpreterExecFlag(family string, args []string) string {
	flags, withArg := interpreterExecFlags[family], interpreterWithArg[family]
	powershell := family == "powershell"
	skipValue := false
	for _, token := range args {
		if skipValue {
			skipValue = false
			continue
		}
		if token == "--" || (!powershell && !strings.HasPrefix(token, "-")) {
			if family == "deno" && strings.ToLower(token) == "eval" {
				return "eval"
			}
			break
		}
		option, equals, _ := partition(token, "=")
		comparable := option
		if powershell {
			comparable = strings.ToLower(option)
		}
		if flags[comparable] {
			return comparable
		}
		hasAttachedOptionValue := false
		for short := range withArg {
			if strings.HasPrefix(short, "-") && !strings.HasPrefix(short, "--") && strings.HasPrefix(option, short) && len(option) > len(short) {
				hasAttachedOptionValue = true
				break
			}
		}
		if !powershell && !strings.HasPrefix(option, "--") && utf8.RuneCountInString(option) > 2 && !hasAttachedOptionValue {
			for _, char := range option[1:] {
				if flags["-"+string(char)] {
					return "-" + string(char)
				}
			}
		}
		skipValue = withArg[comparable] && !equals
	}
	return ""
}

func allRunesIn(s, allowed string) bool {
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}

// Whether bash -c occurs and the command string it owns
func bashExecPayload(args []string) (bool, string, bool) {
	index := 0
	for index < len(args) {
		token := args[index]
		if token == "--" || !(strings.HasPrefix(token, "-") || strings.HasPrefix(token, "+")) {
			break
		}
		if bashOptionsWithArg[token] {
			index += 2
			continue
		}
		chars := token[1:]
		if strings.HasPrefix(token, "--") || !allRunesIn(chars, bashShortOptionLetters) {
			index++
			continue
		}
		consumed := 0
		if strings.ContainsAny(chars, "Oo") {
			consumed = 1
		}
		if strings.Contains(chars, "c") {
			payloadIndex := index + 1 + consumed
			if payloadIndex < len(args) {
				return true, args[payloadIndex], true
			}
			return true, "", false
		}
		index += 1 + consumed
	}
	return false, "", false
}

// Option and program for a read only tool's program running flag
func readToolExecFlag(tool string, args []string) (string, string, bool) {
	flags := readToolExecFlags[tool]
	index := 0
	for index < len(args) {
		token := args[index]
		if token == "--" {
			break
		}
		option, equals, payload := partition(token, "=")
		hasPayload := equals
		matched := ""
		if flags[option] {
			matched = option
		}
		if tool == "man" && (strings.HasPrefix(token, "-P") || strings.HasPrefix(token, "-H")) && utf8.RuneCountInString(token) > 2 {
			matched, payload, hasPayload = token[:2], token[2:], true
		}
		switch {
		case matched != "":
			if !hasPayload && index+1 < len(args) {
				payload, hasPayload = args[index+1], true
			}
			if payload != "" {
				return matched, payload, true
			}
			if hasPayload && !strings.Contains(token, "=") {
				index += 2
			} else {
				index++
			}
		case readToolLongOptionsWithArg[tool][option] && !hasPayload:
			index += 2
		case strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") && utf8.RuneCountInString(token) > 1:
			shorts := readToolShortOptionsWithArg[tool]
			owner := -1
			k := 1
			for _, char := range token[1:] {
				if strings.ContainsRune(shorts, char) {
					owner = k
					break
				}
				k++
			}
			if owner == utf8.RuneCountInString(token)-1 {
				index += 2
			} else {
				index++
			}
		default:
			index++
		}
	}
	return "", "", false
}

type execFinding struct {
	description string
	payload     string
	hasPayload  bool
}

// Scoped execution mechanisms and any executable payloads in the command
func executionFlagFindings(command string) []execFinding {
	var out []execFinding
	for _, segment := range iterTopLevelShellSegments(command) {
		for _, ws := range iterShellCommandWordSpans(segment) {
			executable := deobfuscateShellWordForDetection(ws.word)
			tokens, ok := shellSegmentTokens(segment, ws.start)
			executableName := strings.ToLower(pyBasename(executable))
			family := interpreterFamily(executable)
			if !ok {
				if family != "" || readToolExecFlags[executableName] != nil {
					out = append(out, execFinding{description: MalformedExecDescription})
				}
				continue
			}
			if len(tokens) == 0 {
				continue
			}
			args := tokens[1:]
			heredoc := false
			for _, t := range args {
				if strings.HasPrefix(t, "<<") {
					heredoc = true
					break
				}
			}
			switch {
			case family != "" && interpreterExecFlag(family, args) != "":
				out = append(out, execFinding{description: "script execution via -e/-c flag"})
			case family != "" && heredoc:
				out = append(out, execFinding{description: "script execution via heredoc"})
			default:
				if setOf(shellNames...)[executableName] {
					if found, payload, has := bashExecPayload(args); found {
						out = append(out, execFinding{"shell command via -c/-lc flag", payload, has})
					}
				}
				if readToolExecFlags[executableName] != nil {
					if opt, prog, ok := readToolExecFlag(executableName, args); ok {
						out = append(out, execFinding{"arbitrary program execution via " + executableName + " " + opt, prog, true})
					}
				}
			}
		}
	}
	return out
}

var envSplitEscapes = map[rune]string{
	'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v", '#': "#", '$': "$", '"': "\"", '\'': "'", '\\': "\\",
}

// Projects GNU env -S literal argv, nil when the payload needs dynamic expansion
func splitEnvString(payload string) ([]string, bool) {
	runes := []rune(payload)
	var args []string
	var word strings.Builder
	var quote rune
	started := false
	index := 0
	for index < len(runes) {
		char := runes[index]
		index++
		if char == '\\' {
			if index == len(runes) {
				return nil, false
			}
			escaped := runes[index]
			if quote == '\'' && escaped != '\'' && escaped != '\\' {
				word.WriteRune(char)
				started = true
				continue
			}
			index++
			if escaped == 'c' {
				if quote != 0 {
					return nil, false
				}
				break
			}
			if escaped == '_' && quote == 0 {
				if started {
					args = append(args, word.String())
				}
				word.Reset()
				started = false
				continue
			}
			repl, ok := envSplitEscapes[escaped]
			if !ok && escaped != '_' {
				return nil, false
			}
			if escaped == '_' {
				word.WriteString(" ")
			} else {
				word.WriteString(repl)
			}
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
	if args == nil {
		args = []string{}
	}
	return args, true
}

// Shell quoted command an env -S invocation runs, empty when none
func envSplitPayload(tokens []string) string {
	index := 1
	for index < len(tokens) {
		token := tokens[index]
		if token == "--" || !strings.HasPrefix(token, "-") {
			return ""
		}
		option, equals, value := partition(token, "=")
		if option == "--split-string" || strings.HasPrefix(token, "-S") {
			attached := len(token) > 2
			if option == "--split-string" {
				attached = equals
			}
			if !attached {
				index++
			}
			payload := ""
			if attached {
				if option == "--split-string" {
					payload = value
				} else {
					payload = token[2:]
				}
			} else if index < len(tokens) {
				payload = tokens[index]
			}
			args, ok := splitEnvString(payload)
			if !ok {
				return ""
			}
			rest := []string{}
			if index+1 < len(tokens) {
				rest = tokens[index+1:]
			}
			return shJoin(append(args, rest...))
		}
		if !equals && commandWrapperOptionsWithArg["env"][option] {
			index += 2
		} else {
			index++
		}
	}
	return ""
}

// DenyCommandVariants adds executable projections for user deny rules on top of the detection variants
func DenyCommandVariants(command string) []string {
	out := CommandVariants(command)
	pending := []string{command}
	seen := map[string]bool{}
	for len(pending) > 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[source] {
			continue
		}
		seen[source] = true
		for _, ws := range iterShellCommandWordSpans(source) {
			segment := shellCommandSegment(source, ws.start)
			executable := deobfuscateShellWordForDetection(ws.word)
			tail := ""
			if ws.end-ws.start <= len(segment) {
				tail = segment[ws.end-ws.start:]
			}
			var parts strings.Builder
			lastSpace := false
			for sc := newShellScan(tail, 0, -1, scanOpts{}); ; {
				st, ok := sc.next()
				if !ok {
					break
				}
				r, _ := runeAt(tail, st.i)
				if st.kind == stepChar && st.quote == 0 && pyIsSpace(r) {
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
			if base := pyBasename(executable); base != executable {
				names = append(names, base)
			}
			for _, name := range names {
				candidate := name + tail
				out = append(out, candidate, normalizeCommandForDetection(candidate))
			}
			if pyBasename(executable) == "env" {
				if tokens, ok := shellSegmentTokens(segment, 0); ok && len(tokens) > 0 {
					if payload := envSplitPayload(tokens); payload != "" {
						pending = append(pending, payload)
					}
				}
			}
		}
		for _, f := range executionFlagFindings(source) {
			if f.hasPayload && f.payload != "" {
				pending = append(pending, f.payload)
			}
		}
	}
	return out
}

var (
	windowsPathRe        = regexp.MustCompile(`(?:[A-Za-z]:|\\\\)[\\\\]`)
	detectWindowsDriveRe = regexp.MustCompile(`[A-Za-z]:\\`)
	verificationNameRe   = regexp.MustCompile(`\A(?:icebath|hermes)-(?:verify|ad-hoc)-[A-Za-z0-9_.-]+\z`)
)

// CommandVariants lists every detection spelling of a command in scan order
func CommandVariants(command string) []string {
	normalized := normalizeCommandForDetection(maskQuotedNewlines(command))
	grepSafe, _ := grepSafeDetectionVariant(normalized)
	seen := map[string]bool{grepSafe: true}
	out := []string{grepSafe}
	fresh := func(variant string) bool {
		if variant == "" || seen[variant] {
			return false
		}
		seen[variant] = true
		return true
	}
	if windowsPathRe.MatchString(command) || detectWindowsDriveRe.MatchString(command) {
		winVariant := normalizeCommandForDetection(maskQuotedNewlines(strings.ReplaceAll(command, "\\", "/")))
		if fresh(winVariant) {
			out = append(out, winVariant)
		}
	}
	pending := []string{normalized}
	for len(pending) > 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, f := range executionFlagFindings(source) {
			if !f.hasPayload || !fresh(f.payload) {
				continue
			}
			out = append(out, f.payload)
			marked := markCommandStarts(f.payload, "\n")
			if marked != f.payload && fresh(marked) {
				out = append(out, marked)
			}
			pending = append(pending, f.payload)
		}
	}
	marked := markCommandStarts(grepSafe, "\n")
	if marked != grepSafe && fresh(marked) {
		out = append(out, marked)
	}
	faithful := normalizeCommandForDetection(markCommandStarts(maskQuotedNewlines(command), " \n"))
	if fresh(faithful) {
		out = append(out, faithful)
	}
	var pendingSpans []spliceEdit
	for _, ws := range iterShellCommandWordSpans(normalized) {
		deobfuscated := deobfuscateShellWordForDetection(ws.word)
		if deobfuscated != "" && deobfuscated != ws.word {
			pendingSpans = append(pendingSpans, spliceEdit{ws.start, ws.end, deobfuscated})
		}
	}
	sort.SliceStable(pendingSpans, func(i, j int) bool {
		if pendingSpans[i].start != pendingSpans[j].start {
			return pendingSpans[i].start < pendingSpans[j].start
		}
		return pendingSpans[i].end < pendingSpans[j].end
	})
	for len(pendingSpans) > 0 {
		var applied, carry []spliceEdit
		cursor := 0
		for _, span := range pendingSpans {
			if span.start < cursor {
				carry = append(carry, span)
			} else {
				applied = append(applied, span)
				cursor = span.end
			}
		}
		variant := splice(normalized, applied)
		if fresh(variant) {
			out = append(out, variant)
		}
		pendingSpans = carry
	}
	return out
}

// Reports whether the command only removes one icebath ad hoc temp script
func isVerificationArtifactCleanup(command string) bool {
	argv, err := shlexSplit(command)
	if err != nil {
		return false
	}
	if len(argv) != 3 || argv[0] != "rm" || argv[1] != "-f" {
		return false
	}
	operand := argv[2]
	tempDir := os.TempDir()
	if real, err := filepath.EvalSymlinks(tempDir); err == nil {
		tempDir = real
	}
	basename := pyBasename(operand)
	joined := tempDir + basename
	if !strings.HasSuffix(tempDir, string(filepath.Separator)) {
		joined = tempDir + string(filepath.Separator) + basename
	}
	return operand == joined && filepath.Dir(resolveNonStrict(operand)) == tempDir && verificationNameRe.MatchString(basename)
}

// Catches gateway lifecycle verbs spelled with quote splicing through the token aware lifecycle guard
func isShellTokenSplicedGatewayLifecycle(command string) bool {
	return ContainsGatewayLifecycleCommand(command)
}

// Reports whether any command position word is a shell carrying command
func containsShellCarrier(command string) bool {
	for _, ws := range iterShellCommandWordSpans(command) {
		if shellCarrierNames[strings.ToLower(pyBasename(deobfuscateShellWordForDetection(ws.word)))] {
			return true
		}
	}
	return false
}

// Blanks quoted string content for positionless hardline matching, substitutions inside double quotes stay raw
func maskQuotedProse(command string) string {
	var b strings.Builder
	for sc := newShellScan(command, 0, -1, scanOpts{subst: "q", naiveBacktick: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.quote == 0 || st.kind == stepQuote || st.kind == stepSubst {
			b.WriteString(command[st.i:st.j])
		} else {
			b.WriteString(strings.Repeat(" ", utf8.RuneCountInString(command[st.i:st.j])))
		}
	}
	return b.String()
}

var sudoStdinRe = regexp.MustCompile("(?i)(?:^|[;&|`\n]|&&|\\|\\||\\$\\()\\s*sudo\\s+-S\\b")

// Reports whether SUDO_PASSWORD is configured for the session or the process
func sudoPasswordConfigured(env *Env) bool {
	if env != nil && env.Secrets != nil {
		if _, ok := env.Secrets.Get("SUDO_PASSWORD"); ok {
			return true
		}
	}
	_, ok := os.LookupEnv("SUDO_PASSWORD")
	return ok
}

// CheckSudoStdinGuard blocks an explicit sudo -S when no SUDO_PASSWORD is configured
func CheckSudoStdinGuard(env *Env, command string) (bool, string) {
	if !sudoPasswordConfigured(env) && sudoStdinRe.MatchString(strings.ToLower(normalizeCommandForDetection(command))) {
		return true, "sudo password guessing via stdin (sudo -S)"
	}
	return false, ""
}

// DetectHardlineCommand reports the never bypassable hardline match for a command
func DetectHardlineCommand(command string) (bool, string) {
	if commandParserLimitExceeded(command) {
		return true, ParserLimitDescription
	}
	if _, malformed := grepSafeDetectionVariant(maskQuotedNewlines(command)); malformed {
		return true, MalformedExecDescription
	}
	for _, variant := range CommandVariants(command) {
		variantLower := strings.ToLower(variant)
		masked, maskedSet := "", false
		for _, p := range hardlineCompiled {
			if p.quoteMasked && !maskedSet {
				if containsShellCarrier(variant) {
					masked = variantLower
				} else {
					masked = strings.ToLower(maskQuotedProse(variant))
				}
				maskedSet = true
			}
			target := variantLower
			if p.quoteMasked {
				target = masked
			}
			if regexp2Match(p.re, target) {
				return true, p.description
			}
		}
	}
	return false, ""
}

// Walks every dangerous description in detection order until the callback returns false
func iterDangerousDescriptions(command string, only map[string]bool, each func(string) bool) {
	if commandParserLimitExceeded(command) {
		if only == nil {
			each(ParserLimitDescription)
		}
		return
	}
	if isVerificationArtifactCleanup(command) {
		return
	}
	patterns := dangerousCompiled
	if only != nil {
		patterns = nil
		for _, p := range dangerousCompiled {
			if only[p.description] {
				patterns = append(patterns, p)
			}
		}
	}
	for _, variant := range CommandVariants(command) {
		commandLower := lowerPreservingFlags(variant)
		masked, maskedSet := "", false
		for _, p := range patterns {
			if quoteMaskedDangerousDescriptions[p.description] {
				if !maskedSet {
					masked = lowerPreservingFlags(maskQuotedProse(variant))
					maskedSet = true
				}
				if regexp2Match(p.re, masked) && !each(p.description) {
					return
				}
			} else if regexp2Match(p.re, commandLower) && !each(p.description) {
				return
			}
		}
	}
	if only != nil {
		if only[InvisibleUnicodeDescription] && regexp2Match(hiddenVariationSelectorRe, command) {
			each(InvisibleUnicodeDescription)
		}
		return
	}
	normalized := normalizeCommandForDetection(command)
	for _, f := range executionFlagFindings(normalized) {
		if !each(f.description) {
			return
		}
	}
	if isShellTokenSplicedGatewayLifecycle(command) && !each(GatewayLifecycleSpliceDescription) {
		return
	}
	if regexp2Match(hiddenVariationSelectorRe, command) {
		each(InvisibleUnicodeDescription)
	}
}

// DetectDangerousCommand reports the first dangerous match as its pattern key and description
func DetectDangerousCommand(command string) (string, string, bool) {
	found := ""
	iterDangerousDescriptions(command, nil, func(d string) bool {
		found = d
		return false
	})
	if found == "" {
		return "", "", false
	}
	return found, found, true
}

// DetectContentRuleKeys lists every content level rule the command matches in detection order
func DetectContentRuleKeys(command string) []string {
	var keys []string
	seen := map[string]bool{}
	iterDangerousDescriptions(command, ContentRuleDescriptions, func(d string) bool {
		if !seen[d] {
			seen[d] = true
			keys = append(keys, d)
		}
		return true
	})
	return keys
}

// ShlexSplitCommand splits a command line with POSIX shlex semantics
func ShlexSplitCommand(command string) ([]string, error) {
	return shlexSplit(command)
}

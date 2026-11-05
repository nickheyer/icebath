package mcp

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Shell interpreters whose inline scripts are scanned for egress and persistence
var shellInterpreters = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "fish": true, "cmd": true, "cmd.exe": true,
	"powershell": true, "powershell.exe": true, "pwsh": true, "pwsh.exe": true,
}

var egressPattern = regexp2.MustCompile(
	`(?<![\w.-])(?:curl|wget|nc|ncat|socat)(?![\w.-])`+
		`|/dev/tcp/`+
		`|\bInvoke-WebRequest\b`+
		`|\bInvoke-RestMethod\b`+
		`|\bSystem\.Net\.WebClient\b`, regexp2.IgnoreCase)

var exfilHintPattern = regexp2.MustCompile(`\.env\b|--data-binary|--data-raw|\b-X\s+POST\b|\bPOST\b|<\s*[^\s]+`, regexp2.IgnoreCase)

// OS persistence surfaces an MCP server has no reason to write, the SSH key PAM sudoers cron shape
var persistencePattern = regexp2.MustCompile(
	`authorized_keys`+
		`|\.ssh/`+
		`|/etc/ssh\b`+
		`|/etc/pam\.d\b|pam_[\w-]+\.so`+
		`|/etc/sudoers`+
		`|/etc/cron|crontab\b`+
		`|/etc/rc\.local|/etc/systemd`+
		`|\.bashrc\b|\.bash_profile\b|\.profile\b|\.zshrc\b`, regexp2.IgnoreCase)

// Attacker artifacts of the June 2026 hermes-0day campaign, refused wherever they appear
var iocSubstrings = []string{
	"AAAAC3NzaC1lZDI1NTE5AAAAICBoh1oDC4DnsO1m5mJ4yfEKrQebaFh",
	"hermes-0day",
	"60.165.167.",
	"118.182.244.156",
	"61.178.123.196",
}

// Reports whether a regexp2 pattern matches anywhere in text
func reMatch(re *regexp2.Regexp, text string) bool {
	ok, err := re.MatchString(text)
	return err == nil && ok
}

// Lowercased basename of the first shell word of a command
func commandBasename(command any) string {
	if !pyTruthy(command) {
		command = ""
	}
	text := strings.TrimSpace(pyStr(command))
	if text == "" {
		return ""
	}
	var parts []string
	var err error
	if runtime.GOOS == "windows" {
		parts, err = shlexNonPosix(text)
	} else {
		parts, err = tools.ShlexSplitCommand(text)
	}
	if err != nil {
		parts = strings.Fields(text)
	}
	first := text
	if len(parts) > 0 {
		first = parts[0]
	}
	return strings.ToLower(pyBasename(first))
}

// Words of a command line under non POSIX shlex rules, quotes kept and a quoted token ending at its close
func shlexNonPosix(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !inWord && (c == '"' || c == '\''):
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return nil, errShlexUnclosed
			}
			words = append(words, s[i:i+end+2])
			i += end + 1
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

var errShlexUnclosed = fmt.Errorf("No closing quotation")

// Final path component the way os.path.basename splits it, empty after a trailing separator
func pyBasename(p string) string {
	if runtime.GOOS == "windows" {
		if len(p) >= 2 && p[1] == ':' {
			p = p[2:]
		}
		return p[strings.LastIndexAny(p, `/\`)+1:]
	}
	return p[strings.LastIndex(p, "/")+1:]
}

// Args as one inline script string, a list joined by spaces
func inlineScript(args any) string {
	switch a := args.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, len(a))
		for i, item := range a {
			parts[i] = pyStr(item)
		}
		return strings.Join(parts, " ")
	case []string:
		return strings.Join(a, " ")
	}
	return pyStr(args)
}

// Command plus args plus env values flattened for IOC scanning
func entryText(entry map[string]any) string {
	cmd := entry["command"]
	if !pyTruthy(cmd) {
		cmd = ""
	}
	parts := []string{pyStr(cmd), inlineScript(entry["args"])}
	if env, ok := entry["env"].(map[string]any); ok {
		for _, k := range orderedKeys(env) {
			parts = append(parts, pyStr(env[k]))
		}
	}
	return strings.Join(parts, " ")
}

// Security warnings for one server entry, empty when it is not one of the three blocked shapes
func ValidateServerEntry(name string, entry map[string]any) []string {
	if entry == nil {
		return nil
	}
	var issues []string
	flat := entryText(entry)
	for _, ioc := range iocSubstrings {
		if strings.Contains(flat, ioc) {
			return append(issues, fmt.Sprintf("MCP server '%s' contains a known hermes-0day indicator-of-compromise ('%s')", name, ioc))
		}
	}
	command := entry["command"]
	if !shellInterpreters[commandBasename(command)] {
		return issues
	}
	script := inlineScript(entry["args"])
	if script == "" {
		return issues
	}
	if reMatch(egressPattern, script) {
		issue := fmt.Sprintf("MCP server '%s' uses shell interpreter '%s' with network egress in args", name, pyStr(command))
		if reMatch(exfilHintPattern, script) {
			issue += " and exfiltration-shaped arguments"
		}
		issues = append(issues, issue)
	}
	if reMatch(persistencePattern, script) {
		issues = append(issues, fmt.Sprintf("MCP server '%s' uses shell interpreter '%s' to write to an OS persistence surface "+
			"(SSH keys / PAM / sudoers / cron / shell rc) \u2014 this is the hermes-0day backdoor shape, not a real MCP server", name, pyStr(command)))
	}
	return issues
}

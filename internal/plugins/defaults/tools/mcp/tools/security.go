package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// Shell interpreters whose inline scripts are scanned for egress and persistence
var shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "fish": true, "cmd": true, "cmd.exe": true, "powershell": true,
	"powershell.exe": true, "pwsh": true, "pwsh.exe": true}

var (
	egress = regexp2.MustCompile(`(?<![\w.-])(?:curl|wget|nc|ncat|socat)(?![\w.-])|/dev/tcp/|\bInvoke-WebRequest\b|\bInvoke-RestMethod\b|\bSystem\.Net\.WebClient\b`,
		regexp2.IgnoreCase)
	exfiltration = regexp2.MustCompile(`\.env\b|--data-binary|--data-raw|\b-X\s+POST\b|\bPOST\b|<\s*[^\s]+`, regexp2.IgnoreCase)
	// Surfaces of the OS an MCP server has no reason to write: SSH keys, PAM, sudoers, cron, shell rc files
	persistence = regexp2.MustCompile(`authorized_keys|\.ssh/|/etc/ssh\b|/etc/pam\.d\b|pam_[\w-]+\.so|/etc/sudoers|/etc/cron|crontab\b|/etc/rc\.local|`+
		`/etc/systemd|\.bashrc\b|\.bash_profile\b|\.profile\b|\.zshrc\b`, regexp2.IgnoreCase)
)

// Artifacts of the June 2026 MCP backdoor campaign, refused wherever they appear
var compromise = []string{"AAAAC3NzaC1lZDI1NTE5AAAAICBoh1oDC4DnsO1m5mJ4yfEKrQebaFh", "hermes-0day", "60.165.167.", "118.182.244.156", "61.178.123.196"}

func found(re *regexp2.Regexp, text string) bool {
	ok, err := re.MatchString(text)
	return err == nil && ok
}

// Problems of a server's settings that make it a backdoor rather than a server: the campaign's artifacts, or a shell script reaching the
// network or writing persistence surfaces. None for a server of no such shape
func validate(name string, entry map[string]any) []string {
	command := strings.TrimSpace(fmt.Sprint(orNil(entry["command"])))
	script := strings.Join(Args(entry["args"]), " ")
	flat := []string{command, script}
	for _, v := range envOf(entry["env"]) {
		flat = append(flat, v)
	}
	text := strings.Join(flat, " ")
	for _, artifact := range compromise {
		if strings.Contains(text, artifact) {
			return []string{fmt.Sprintf("The MCP server %s holds “%s”, a known sign of the MCP backdoor attacks of June 2026.", name, artifact)}
		}
	}
	first := command
	if words := strings.Fields(command); len(words) > 0 {
		first = strings.Trim(words[0], `"'`)
	}
	if !shells[strings.ToLower(filepath.Base(strings.ReplaceAll(first, `\`, "/")))] || script == "" {
		return nil
	}
	var issues []string
	if found(egress, script) {
		issue := fmt.Sprintf("The MCP server %s runs the shell %s with arguments that reach the network.", name, command)
		if found(exfiltration, script) {
			issue = fmt.Sprintf("The MCP server %s runs the shell %s with arguments that reach the network and look like they send your data away.", name,
				command)
		}
		issues = append(issues, issue)
	}
	if found(persistence, script) {
		issues = append(issues, fmt.Sprintf("The MCP server %s runs the shell %s to write where programs start by themselves, such as SSH keys, PAM, "+
			"sudoers, cron or the startup files of your shell. That is how the MCP backdoor attacks of June 2026 work, not what an MCP server does.", name,
			command))
	}
	return issues
}

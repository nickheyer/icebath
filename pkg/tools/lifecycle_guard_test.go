package tools

import (
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

var gatewayLifecycleLaunchctl = []string{
	"launchctl kickstart -k gui/501/ai.icebath.gateway",
	"launchctl unload ~/Library/LaunchAgents/ai.icebath.gateway.plist",
	"launchctl load ~/Library/LaunchAgents/ai.icebath.gateway.plist",
	"launchctl stop ai.icebath.gateway",
	"launchctl restart ai.icebath.gateway",
	"launchctl bootout gui/501/ai.icebath.gateway",
	"launchctl remove ai.icebath.gateway",
	"launchctl disable gui/501/ai.icebath.gateway",
	"launchctl stop ai.hermes.gateway",
}

func TestLifecycleGuardLaunchctlParity(t *testing.T) {
	for _, cmd := range gatewayLifecycleLaunchctl {
		if !ContainsGatewayLifecycleCommand(cmd) {
			t.Errorf("hard block must cover %q", cmd)
		}
		if _, flagged := dangerous(t, cmd); flagged && !ContainsGatewayLifecycleCommand(cmd) {
			t.Errorf("approval layer flags %q but the hard block does not", cmd)
		}
	}
	for _, cmd := range []string{
		"launchctl bootout gui/501/com.example.unrelated", "launchctl remove ai.icebath.update-checker",
		"launchctl disable gui/501/com.apple.WindowServer", "launchctl print system/com.apple.WindowServer",
	} {
		if ContainsGatewayLifecycleCommand(cmd) {
			t.Errorf("unrelated label blocked: %q", cmd)
		}
	}
}

func TestGatewayLifecycleShapes(t *testing.T) {
	for _, cmd := range []string{
		"icebath gateway restart", "hermes gateway stop", "icebath.exe gateway uninstall",
		"systemctl --user restart icebath-gateway", "pkill -f 'icebath gateway'", "taskkill /F /FI \"imagename eq icebath\" gateway",
		`launchctl kick"start" -k gui/501/ai.icebath.gateway`, "launchctl \\\n  bootout gui/501/ai.icebath.gateway",
		`subprocess.run(["launchctl", "bootout", "gui/501/ai.icebath.gateway"])`,
		"label=ai.icebath.gateway; launchctl bootout \"gui/501/$label\"",
		"cat <<'EOF'\nicebath gateway restart\nEOF\nicebath gateway restart",
		"pkill -9 python3", "taskkill /F /IM python.exe", "killall python", "pgrep python | xargs kill", "Get-Process python | Stop-Process",
	} {
		if !ContainsGatewayLifecycleCommand(cmd) {
			t.Errorf("lifecycle shape missed: %q", cmd)
		}
	}
	for _, cmd := range []string{
		"icebath gateway start", "/docs/icebath gateway restart-notes.md", "echo restart the payment gateway",
		"cat > runbook.md <<'EOF'\nicebath gateway restart\nEOF", "pkill -f nginx", "pkill -u bob chrome",
		"taskkill /F /IM chrome.exe", "python mt_add.py",
	} {
		if ContainsGatewayLifecycleCommand(cmd) {
			t.Errorf("benign shape blocked: %q", cmd)
		}
	}
	if !ContainsLaunchctlSubmitCommand("sudo launchctl submit -l com.neutral -- /tmp/helper.sh") || ContainsLaunchctlSubmitCommand("echo 'launchctl submit'") {
		t.Fatal("launchctl submit detection")
	}
	if DirectLifecycleScan("grep 'icebath gateway restart' notes.md") || !DirectLifecycleScan("grep x f | bash -c 'icebath gateway restart'") {
		t.Fatal("data sink exemption")
	}
}

func TestRedactTerminalOutput(t *testing.T) {
	env := &Env{Home: t.TempDir(), Config: config.Tree{}, Values: &sync.Map{}}
	out := RedactTerminalCommandOutput(env, "OPENAI_API_KEY=sk-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234\nMAX_TOKENS=100", "env", false)
	if contains(out, "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234") || !contains(out, "MAX_TOKENS=100") {
		t.Fatalf("env dump redaction %q", out)
	}
	source := "MAX_TOKENS=100\nSERVICE_TOKEN=abc123randomstringvalue"
	if got := RedactTerminalCommandOutput(env, source, "cat main.py", false); got != source {
		t.Fatalf("source dump mangled %q", got)
	}
	if got := RedactTerminalCommandOutput(env, "E   {'BRAVE_API_KEY': 'BSAabcdefghijklmnopqrstuvwxyz'}", "pytest", false); contains(got, "BSAabcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("pytest diagnostic line leaked %q", got)
	}
	if got := RedactSensitiveText("Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456"); contains(got, "abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("auth header leaked %q", got)
	}
	if got := RedactForEgress(env, "the bearer of bad news"); got != "the bearer of bad news" {
		t.Fatalf("prose bearer changed %q", got)
	}
	if got := RedactForEgress(env, "Bearer abcdefghijklmnopqrstuvwxyz0123456789"); got != "Bearer [redacted]" {
		t.Fatalf("egress bearer %q", got)
	}
	if !IsEnvDumpCommand("printenv | sort") || IsEnvDumpCommand("cat file") || !commandReadsSecretFile(env, "cat ~/.bashrc") || commandReadsSecretFile(env, "grep .bashrc app.py") {
		t.Fatal("command classification")
	}
	strict := RedactSensitiveTextWith(env, "https://u:p@host/x?access_token=abc&q=1", RedactOptions{URLCredentials: true})
	if contains(strict, "u:p@") || contains(strict, "access_token=abc") || !contains(strict, "q=1") {
		t.Fatalf("strict url redaction %q", strict)
	}
	identity := CommandApprovalIdentity(env, &Call{Args: map[string]any{"command": "rm -rf /tmp/x &"}})
	if identity == "" || identity != CommandApprovalIdentity(env, &Call{Args: map[string]any{"command": "rm -rf /tmp/x &"}}) {
		t.Fatal("identity must be stable")
	}
	if identity == CommandApprovalIdentity(env, &Call{Args: map[string]any{"command": "icebath gateway run &"}}) {
		t.Fatal("identity must separate pattern keys")
	}
}

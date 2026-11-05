package tools

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func denyEnv(t *testing.T, patterns ...string) *Env {
	t.Helper()
	list := make([]any, len(patterns))
	for i, p := range patterns {
		list[i] = p
	}
	return &Env{Home: t.TempDir(), Config: config.Tree{"approvals": map[string]any{"mode": "manual", "deny": list}}}
}

func TestMatchUserDenyRuleBasics(t *testing.T) {
	if got := matchUserDenyRule(denyEnv(t), "git push --force origin main"); got != "" {
		t.Fatalf("empty deny list matched %q", got)
	}
	noKey := &Env{Home: t.TempDir(), Config: config.Tree{"approvals": map[string]any{"mode": "manual"}}}
	if got := matchUserDenyRule(noKey, "rm -rf build/"); got != "" {
		t.Fatalf("missing deny key matched %q", got)
	}
	if got := matchUserDenyRule(nil, "git push --force"); got != "" {
		t.Fatalf("nil env matched %q", got)
	}
	if got := matchUserDenyRule(denyEnv(t, "git push --force*"), `git pu""sh --force origin main`); got == "" {
		t.Fatal("quote obfuscation must still match")
	}
}

func TestDenyFollowsExecutableIdentity(t *testing.T) {
	env := denyEnv(t, "sudo *", "printf SAFE")
	for _, command := range []string{
		"sudo -n id -u", "/usr/bin/sudo -n id -u", "./sudo -n id -u",
		`"/usr/bin/su"do -n id -u`, `/usr/bin/sud\o -n id -u`,
		"FOO=bar /usr/bin/sudo -n id -u", "env sudo -n id -u",
		"/usr/bin/env -i FOO=bar /usr/bin/sudo -n id -u",
		"env -u FOO -- /usr/bin/sudo -n id -u",
		"env -C /tmp /usr/bin/sudo -n id -u",
		"command -p /usr/bin/sudo -n id -u", "exec -a label /usr/bin/sudo -n id -u",
		"nohup /usr/bin/sudo -n id -u", "nice -n 5 /usr/bin/sudo -n id -u",
		"timeout 5 /usr/bin/sudo -n id -u",
		"setsid -f /usr/bin/sudo -n id -u", "time -p /usr/bin/sudo -n id -u",
		"stdbuf --output L /usr/bin/sudo -n id -u",
		"ionice --class 2 /usr/bin/sudo -n id -u",
		"chrt --fifo 20 /usr/bin/sudo -n id -u",
		"taskset --cpu-list 0 /usr/bin/sudo -n id -u",
		"chroot --userspec root:root /srv /usr/bin/sudo -n id -u",
		"true && /usr/bin/sudo -n id -u; echo ok",
		"echo ok | /usr/bin/sudo -n id -u", "(/usr/bin/sudo -n id -u)",
		`echo "$(/usr/bin/sudo -n id -u)"`,
		"bash -lc 'env -i /usr/bin/sudo -n id -u'",
		"env -S '/usr/bin/sudo -n id -u'",
		"env -S /usr/bin/sudo -n id -u",
		"env --split-string=/usr/bin/sudo -n id -u",
		"env -S \"bash -c '/usr/bin/sudo -n id -u'\"",
		"echo ok # ignored\n bash -c '/usr/bin/sudo -n id -u'",
		"printf SAFE", "env -S printf SAFE", "env -Sprintf SAFE",
		"env --split-string=printf SAFE", "env -S 'printf' SAFE",
		`env -S 'printf\_SAFE'`, `env -S 'printf\_SAFE\c ignored'`,
		"env -S 'printf SAFE # ignored'", "env -S 'printf # ignored' SAFE",
		"env -a marker printf SAFE", "env --argv0 marker printf SAFE",
		"env --argv0=marker printf SAFE", "env -amarker printf SAFE",
		`env -a marker -S 'printf\_SAFE'`,
		`env -S '"printf"\_SAFE'`, `env -S "'printf' SAFE"`,
		`env -S 'printf\_\_SAFE'`, `env -S 'printf\c ignored' SAFE`,
		"2>/tmp/log FOO=bar /usr/bin/sudo -n id -u",
		"if true; then /usr/bin/sudo -n id -u; fi",
	} {
		if matchUserDenyRule(env, command) == "" {
			t.Errorf("deny rule missed %q", command)
		}
	}
}

func TestDenyProjectionPreservesDataAndPathRules(t *testing.T) {
	env := denyEnv(t, "/usr/bin/sudo -n id -u")
	if matchUserDenyRule(env, "env /usr/bin/sudo -n id -u; true") == "" {
		t.Fatal("full path deny must match through env")
	}
	if got := matchUserDenyRule(env, "/opt/bin/sudo -n id -u"); got != "" {
		t.Fatalf("different path matched %q", got)
	}
	env = denyEnv(t, "sudo", "sudo *", "git status")
	for _, command := range []string{
		`echo "sudo -n id -u"`, "printf 'ok && sudo -n id -u'",
		`printf "%s" "$(printf safe) sudo -n id -u"`,
		"command -v sudo", "command -V sudo", "command -pv sudo",
		"env -u sudo printf ok", "exec -a sudo printf ok",
		"env -a sudo printf ok", "env --argv0 sudo printf ok",
		`env -S 'printf %s\_sudo\_-n\_id'`,
		"env -S 'printf %s \"sudo\\_-n\\_id\"'",
		`env -S 'printf ok\c bash -c sudo'`,
		"env -S 'printf ok # bash -c sudo'",
		`env -S 'printf %s \${IGNORED}'`,
		"ionice --pid sudo", "chrt --pid sudo", "taskset --pid 1 sudo",
		"echo 'first\nsudo -n id -u'", "echo ok # ; sudo -n id -u",
		"env -S 'printf %s; sudo -n id'",
		"env -S 'printf %s' 'sudo -n id'",
		"env -S 'printf %s' '$(sudo -n id)'",
		"echo ok # ; bash -c '/usr/bin/sudo -n id'",
		"echo ok # unmatched '\n printf ok",
		"printf '%s' '# ; bash -c sudo'",
		"git log --grep='git status'", `printf "%s" "a\"; sudo -n id -u"`,
	} {
		if got := matchUserDenyRule(env, command); got != "" {
			t.Errorf("%q matched deny rule %q", command, got)
		}
	}
	for _, command := range []string{"env git status; echo ok", "(git status)", "git\tstatus # comment",
		"env -S git status", "env -S 'git' status", "env -Sgit status", "env --split-string=git status"} {
		if got := matchUserDenyRule(env, command); got != "git status" {
			t.Errorf("%q: got %q want git status", command, got)
		}
	}
	if got := matchUserDenyRule(env, `env git st""atus`); got != "git status" {
		t.Fatalf("spliced git status: %q", got)
	}
	env = denyEnv(t, `printf "a  b"`)
	if matchUserDenyRule(env, `env printf "a  b"`) == "" {
		t.Fatal("quoted double space must be preserved")
	}
	if got := matchUserDenyRule(env, `env printf "a b"`); got != "" {
		t.Fatalf("single space matched %q", got)
	}
}

func TestSplitEnvString(t *testing.T) {
	cases := []struct {
		literal string
		want    []string
	}{
		{`a\_b`, []string{"a", "b"}}, {`"a\_b"`, []string{"a b"}},
		{`'a\_b'`, []string{`a\_b`}}, {`a\cb ignored`, []string{"a"}},
		{"a # ignored", []string{"a"}}, {"a#b", []string{"a#b"}}, {`\#a`, []string{"#a"}},
		{`\${NAME}`, []string{"${NAME}"}}, {"'${NAME}'", []string{"${NAME}"}},
		{`'a\'b'`, []string{"a'b"}}, {`'a\\b'`, []string{`a\b`}},
		{`a\"b`, []string{`a"b`}}, {`"" a`, []string{"", "a"}},
		{`a\fb`, []string{"a\fb"}}, {`a\nb`, []string{"a\nb"}}, {`a\rb`, []string{"a\rb"}},
		{`a\tb`, []string{"a\tb"}}, {`a\vb`, []string{"a\vb"}},
	}
	denyEnvSudo := denyEnv(t, "sudo *")
	for _, c := range cases {
		got, ok := splitEnvString(c.literal)
		if !ok || len(got) != len(c.want) {
			t.Fatalf("%q: got %q ok=%v want %q", c.literal, got, ok, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: got %q want %q", c.literal, got, c.want)
			}
		}
		command := "env -S " + shQuote("printf %s "+c.literal)
		if m := matchUserDenyRule(denyEnvSudo, command); m != "" {
			t.Fatalf("%q matched %q", command, m)
		}
	}
	for _, unresolved := range []string{"${NAME}", `"${NAME}"`, `"a\cb"`, `a\qb`, "'unclosed"} {
		if _, ok := splitEnvString(unresolved); ok {
			t.Fatalf("%q should be unresolved", unresolved)
		}
	}
}

func TestAllowlistShellOperator(t *testing.T) {
	for _, c := range []string{"ls -la", "cargo bench -- '^a(b|c)$'", "echo $HOME", `git commit -m "a;b"`, "printf 'a|b'"} {
		if hasAllowlistShellOperator(c) {
			t.Errorf("%q should be simple", c)
		}
	}
	for _, c := range []string{"ls; rm x", "a && b", "echo $(id)", `echo "$(id)"`, "cat < f", "bash -c 'a; b'", "node -e 'x|y'", "echo 'unterminated"} {
		if !hasAllowlistShellOperator(c) {
			t.Errorf("%q should be compound", c)
		}
	}
}

func TestBlockResults(t *testing.T) {
	deny := userDenyBlockResult("git push --force*")
	if deny.Approved || deny.PatternKey != "user_deny:git push --force*" || deny.Description != "git push --force*" {
		t.Fatalf("deny result %+v", deny)
	}
	env := &Env{Home: t.TempDir(), Config: config.Tree{}}
	hard := hardlineBlockResult(env, ParserLimitDescription, "echo hi")
	if hard.Approved || hard.PatternKey != "hardline" || hard.Description != ParserLimitDescription {
		t.Fatalf("hardline result %+v", hard)
	}
	if !contains(hard.Message, "Your command was saved to ") {
		t.Fatalf("parser limit block must save the payload: %s", hard.Message)
	}
	plain := hardlineBlockResult(env, "fork bomb", ":(){ :|:& };:")
	if contains(plain.Message, "RECOVERY") {
		t.Fatalf("plain hardline block carries recovery text: %s", plain.Message)
	}
	sudo := sudoStdinBlockResult("sudo password guessing via stdin (sudo -S)")
	if sudo.Approved || !contains(sudo.Message, "Do not pipe passwords") {
		t.Fatalf("sudo result %+v", sudo)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

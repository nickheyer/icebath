package tools

import (
	"strings"
	"testing"
)

func TestStripInertHeredocBodies(t *testing.T) {
	amp := "&"
	inert := "osascript <<'EOF'\nset out to \"count \" " + amp + " (count of items)\nEOF"
	if got := StripInertHeredocBodies(inert); strings.Contains(got, amp) {
		t.Fatalf("quoted inert body still visible: %q", got)
	}
	visible := "cat <<EOF\nfoo " + amp + " bar\nEOF"
	if got := StripInertHeredocBodies(visible); got != visible {
		t.Fatalf("unquoted body was masked: %q", got)
	}
	shell := "bash <<'EOF'\nnohup sleep 10 " + amp + "\nEOF"
	if got := StripInertHeredocBodies(shell); got != shell {
		t.Fatalf("shell consumer body was masked: %q", got)
	}
	multi := "python3 - <<'A' 3<<'B'\none " + amp + " two\nA\nthree " + amp + " four\nB"
	got := StripInertHeredocBodies(multi)
	if strings.Contains(got, amp) || strings.Count(got, "\n") != strings.Count(multi, "\n") {
		t.Fatalf("two quoted bodies not masked with newlines kept: %q", got)
	}
}

func TestPtyQueryResponder(t *testing.T) {
	r := NewPtyQueryResponder(24, 80)
	out1, resp1 := r.Process([]byte("before\x1b["))
	out2, resp2 := r.Process([]byte("5n\x1b[18t\x1b[6n\x1b[?1049$p\x1b[31mafter"))
	if string(append(resp1, resp2...)) != "\x1b[0n\x1b[8;24;80t\x1b[1;1R\x1b[?1049;0$y" {
		t.Fatalf("replies %q %q", resp1, resp2)
	}
	if string(out1)+string(out2)+string(r.Flush()) != "before\x1b[31mafter" {
		t.Fatalf("output %q %q", out1, out2)
	}
	r = NewPtyQueryResponder(24, 80)
	out, resp := r.Process([]byte("\x1b[6\x1b[5n"))
	if string(out) != "\x1b[6" || string(resp) != "\x1b[0n" {
		t.Fatalf("fresh esc abort %q %q", out, resp)
	}
	r = NewPtyQueryResponder(24, 80)
	seq := []byte("\x1b[?12345678901$p")
	out, resp = r.Process(seq)
	if len(resp) != 0 || string(out)+string(r.Flush()) != string(seq) {
		t.Fatalf("oversized mode passthrough %q %q", out, resp)
	}
}

func TestAnnotateHints(t *testing.T) {
	if !strings.Contains(AnnotateFailure("python x.py", 127, "/usr/bin/bash: line 1: python: command not found"), "python3") {
		t.Fatal("bare python hint missing")
	}
	if !strings.Contains(AnnotateFailure("./run.sh", 126, ""), "126") {
		t.Fatal("exit code hint missing")
	}
	if AnnotateFailure("noop", 1, strings.Repeat("x", 5000)+"\npython: command not found") != "" {
		t.Fatal("pattern beyond scan window matched")
	}
	fail := "error[E0308]: mismatched types\nerror: could not compile `x` due to 11 previous errors\n"
	if AnnotateMaskedSuccess("cargo build --release 2>&1 | tail -20", fail) == "" {
		t.Fatal("pipe to tail not flagged")
	}
	if !strings.Contains(AnnotateMaskedSuccess("cargo build || echo \"BUILD FAILED\"", "error: could not compile `x`\nBUILD FAILED\n"), "||") {
		t.Fatal("or fallback not flagged")
	}
	if AnnotateMaskedSuccess("grep error log | tail", fail) != "" {
		t.Fatal("readonly head flagged")
	}
}

func TestRewriteCompoundBackground(t *testing.T) {
	cases := map[string]string{
		"A && B &":                      "A && { B & }",
		"A || B &":                      "A || { B & }",
		"A && B &\nfalse || C &":        "A && { B & }\nfalse || { C & }",
		"sleep 5 &":                     "sleep 5 &",
		"echo hi &>/dev/null":           "echo hi &>/dev/null",
		"A && B 2>&1 &":                 "A && { B 2>&1 & }",
		"echo 'A && B &'":               "echo 'A && B &'",
		"# A && B &\nC":                 "# A && B &\nC",
		"A\t&&\tB\t&":                   "A\t&&\t{ B\t& }",
		"echo hi && sleep 5 & echo done": "echo hi && { sleep 5 & } ; echo done",
	}
	for in, want := range cases {
		if got := RewriteCompoundBackground(in); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
	once := RewriteCompoundBackground("A && B &")
	if RewriteCompoundBackground(once) != once {
		t.Fatal("rewrite is not idempotent")
	}
}

func TestSudoRewrite(t *testing.T) {
	got, n := rewriteRealSudoInvocations("sudo apt update && sudo apt upgrade -y")
	if n != 2 || got != "sudo -S -p '' apt update && sudo -S -p '' apt upgrade -y" {
		t.Fatalf("got %q (%d)", got, n)
	}
	got, n = rewriteRealSudoInvocations("echo sudo && FOO=1 env -u X sudo ls")
	if n != 1 || !strings.Contains(got, "env -u X sudo -S -p '' ls") || strings.Contains(got, "echo sudo -S") {
		t.Fatalf("env chain rewrite %q (%d)", got, n)
	}
	if _, n = rewriteRealSudoInvocations("echo 'sudo rm' | cat"); n != 0 {
		t.Fatal("quoted sudo counted")
	}
}

func TestMacosOpenTransform(t *testing.T) {
	if TransformMacosOpenCommand("open file.pdf", "Linux") != "open file.pdf" {
		t.Fatal("non darwin transformed")
	}
	got := TransformMacosOpenCommand("open -a Preview file.pdf", "Darwin")
	if !strings.HasPrefix(got, "open -a Preview file.pdf; _H_APP=Preview; ") || !strings.Contains(got, "lsappinfo setfront") {
		t.Fatalf("ladder missing: %q", got)
	}
	for _, c := range []string{"open", "open -R file.pdf", "open -a Preview", "ls"} {
		if TransformMacosOpenCommand(c, "Darwin") != c {
			t.Fatalf("%q transformed", c)
		}
	}
}

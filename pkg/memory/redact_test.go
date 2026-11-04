package memory

import (
	"strings"
	"testing"
)

func TestRedactPrefixTokens(t *testing.T) {
	out := RedactForEgress("key ghp_abcdefghijklmnopqrstuvwxyz0123 ok")
	if strings.Contains(out, "ghp_abcdefghijklmnopqrstuvwxyz0123") || !strings.Contains(out, "ghp_ab...0123") {
		t.Fatalf("prefix token: %q", out)
	}
	if out := RedactForEgress("xghp_abcdefghijklmnopqrstuvwxyz0123"); !strings.Contains(out, "xghp_abcdefghijklmnopqrstuvwxyz0123") {
		t.Fatalf("word edge ignored: %q", out)
	}
	if out := RedactForEgress("sk-pro...EFGH stays"); out != "sk-pro...EFGH stays" {
		t.Fatalf("display mask re-masked: %q", out)
	}
}

func TestRedactAssignmentsAndHeaders(t *testing.T) {
	out := RedactForEgress("OPENAI_API_KEY=abcdefghijklmnopqrstu MAX_TOKENS=100")
	if strings.Contains(out, "abcdefghijklmnopqrstu") || !strings.Contains(out, "MAX_TOKENS=100") {
		t.Fatalf("env assignment: %q", out)
	}
	if out := RedactForEgress("SSH_AUTH_SOCK=$HOME/.ssh/agent.sock"); out != "SSH_AUTH_SOCK=$HOME/.ssh/agent.sock" {
		t.Fatalf("path value masked: %q", out)
	}
	out = RedactForEgress("Authorization: Bearer abcdefghijklmnopqrstuvwxyz")
	if !strings.Contains(out, "Authorization: Bearer abcdef...wxyz") {
		t.Fatalf("auth header: %q", out)
	}
	out = RedactForEgress(`{"apiKey": "abcdefghijklmnopqrstuvwx"}`)
	if strings.Contains(out, "abcdefghijklmnopqrstuvwx") {
		t.Fatalf("json field: %q", out)
	}
	if out := RedactForEgress("Secretary: J.Smith"); out != "Secretary: J.Smith" {
		t.Fatalf("prose word masked: %q", out)
	}
}

func TestRedactEgressShapes(t *testing.T) {
	out := RedactForEgress("postgres://user:hunter2@db/app")
	if !strings.Contains(out, "postgres://user:***@db/app") {
		t.Fatalf("db connstr: %q", out)
	}
	if out := RedactForEgress("the bearer of bad news"); out != "the bearer of bad news" {
		t.Fatalf("bearer prose: %q", out)
	}
	if out := RedactForEgress("Bearer abcdefghijklmnopqrstuvwxyz1234"); out != "Bearer [redacted]" {
		t.Fatalf("bare bearer: %q", out)
	}
	if out := RedactForEgress("call +14155550123 now"); out != "call +141****0123 now" {
		t.Fatalf("phone: %q", out)
	}
	if out := RedactForEgress("-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"); out != "[REDACTED PRIVATE KEY]" {
		t.Fatalf("private key: %q", out)
	}
}

func TestRedactForProviderLearnsMaskedTokens(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123"
	vals := RedactForProvider("tool said "+secret, map[string]any{"prose": "bare " + secret + " repeated", "img": "data:image/png;base64,AAAA"})
	if strings.Contains(vals[0].(string), secret) {
		t.Fatalf("first value leaked: %v", vals[0])
	}
	m := vals[1].(map[string]any)
	if strings.Contains(m["prose"].(string), secret) || m["img"] != "data:image/png;base64,AAAA" {
		t.Fatalf("walk: %v", m)
	}
}

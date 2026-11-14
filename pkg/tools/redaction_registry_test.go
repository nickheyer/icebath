package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

const nvapiKey = "nvapi-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcdEFGH"
const nvapiPattern = `nvapi-[A-Za-z0-9_-]{20,}`

func cleanRedactionRegistry(t *testing.T) {
	ResetPluginRedactionPatterns()
	t.Cleanup(ResetPluginRedactionPatterns)
}

func forceRedact(text string) string {
	return RedactSensitiveTextWith(nil, text, RedactOptions{Force: true})
}

func mustRegister(t *testing.T, patterns any, want int) {
	t.Helper()
	got, err := RegisterRedactionPatterns(patterns, "test")
	if err != nil || got != want {
		t.Fatalf("RegisterRedactionPatterns(%v) = %d, %v, want %d", patterns, got, err, want)
	}
}

func TestRedactionRegistryBaselineAndMasking(t *testing.T) {
	cleanRedactionRegistry(t)
	if out := forceRedact("connect failed: " + nvapiKey); !strings.Contains(out, nvapiKey) {
		t.Fatalf("unregistered nvapi key redacted: %q", out)
	}
	mustRegister(t, []string{nvapiPattern}, 1)
	out := forceRedact("connect failed: " + nvapiKey)
	if strings.Contains(out, nvapiKey) || !strings.Contains(out, "nvapi-") || !strings.Contains(out, "...") {
		t.Fatalf("registered pattern did not mask with head and tail: %q", out)
	}
	sk := "sk-proj-AbCdEf1234567890GhIjKl"
	if out := forceRedact("key=" + sk); strings.Contains(out, sk) {
		t.Fatalf("builtin prefix lost after registration: %q", out)
	}
	out = RedactSensitiveTextWith(nil, "api_base_key: "+nvapiKey, RedactOptions{Force: true, FileRead: true})
	if strings.Contains(out, nvapiKey) || !strings.Contains(out, "«redacted:nvapi-…»") {
		t.Fatalf("file read sentinel lacks plugin label: %q", out)
	}
	prose := "the nvapi-endpoint docs describe rate limits"
	if out := forceRedact(prose); out != prose {
		t.Fatalf("prose redacted: %q", out)
	}
	ResetPluginRedactionPatterns()
	if out := forceRedact("x " + nvapiKey); !strings.Contains(out, nvapiKey) {
		t.Fatalf("reset kept plugin pattern: %q", out)
	}
	if out := forceRedact(sk); strings.Contains(out, sk) {
		t.Fatalf("builtin lost after reset: %q", out)
	}
}

func TestRedactionRegistryRejectsAndDedupes(t *testing.T) {
	cleanRedactionRegistry(t)
	mustRegister(t, []string{`nvapi-[unclosed`}, 0)
	if out := forceRedact("x " + nvapiKey); !strings.Contains(out, nvapiKey) {
		t.Fatalf("invalid pattern registered: %q", out)
	}
	for _, p := range []string{`.*secret.*`, `[A-Za-z0-9]{30,}`, `x[A-Za-z0-9]{30,}`} {
		mustRegister(t, []string{p}, 0)
	}
	mustRegister(t, []string{nvapiPattern}, 1)
	mustRegister(t, []string{nvapiPattern}, 0)
	mustRegister(t, []string{prefixPatterns[0]}, 0)
	ResetPluginRedactionPatterns()
	mustRegister(t, []string{nvapiPattern, nvapiPattern}, 1)
	ResetPluginRedactionPatterns()
	mustRegister(t, []any{nil, "", "   ", 42}, 0)
	mustRegister(t, nil, 0)
	if _, err := RegisterRedactionPatterns([]string{"ab{4294967295}"}, "test"); err == nil || err.Error() != "the repetition number is too large" {
		t.Fatalf("overflow repeat must raise, got %v", err)
	}
	if _, err := RegisterRedactionPatterns(42, "test"); err == nil || err.Error() != "'int' object is not iterable" {
		t.Fatalf("non iterable patterns error %v", err)
	}
}

func TestRedactionRegistryStructuralGuards(t *testing.T) {
	cleanRedactionRegistry(t)
	mustRegister(t, []string{`ab|.*`}, 0)
	mustRegister(t, []string{`ab|cd`}, 0)
	clean := "nothing here resembles a credential"
	if out := forceRedact(clean); out != clean {
		t.Fatalf("alternation leaked into matcher: %q", out)
	}
	mustRegister(t, []string{`zq(?:tok|key)-[A-Za-z0-9]{20,}`}, 1)
	mustRegister(t, []string{`xy\|[A-Za-z0-9]{20,}`}, 1)
	mustRegister(t, []string{`wv[|][A-Za-z0-9]{20,}`}, 1)
	for _, p := range []string{`ab(a+)+`, `ab(?:x*)*`, `ab(x{2,})+`, `ab((x+)y)*`} {
		mustRegister(t, []string{p}, 0)
	}
	if out := forceRedact(clean); out != clean {
		t.Fatalf("nested repeat leaked into matcher: %q", out)
	}
	mustRegister(t, []string{`zr(?:tok)?-[A-Za-z0-9]{20,}`}, 1)
	mustRegister(t, []string{`qm-[a-z]+[0-9]+`}, 1)
	mustRegister(t, []string{`pv(?:x{1,5}y)+`}, 1)
	mustRegister(t, []string{`tw[+*][A-Za-z0-9]{20,}`}, 1)
	if alt, nested := patternStructure(`ab[(]x\(y`); alt || nested {
		t.Fatalf("class and escaped parens misread: %v %v", alt, nested)
	}
}

func TestVaultRedactionScopedPerProfile(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ctxA, ctxB := home.WithOverride(context.Background(), a), home.WithOverride(context.Background(), b)
	t.Cleanup(func() { ClearVaultRedactionValues(ctxA); ClearVaultRedactionValues(ctxB) })
	RegisterVaultRedactionValueIn(ctxA, "hunter2\r\npass")
	envA, envB := &Env{Home: a}, &Env{Home: b}
	if got := redactRegisteredVaultValues(envA, "x hunter2\r\npass y hunter2pass"); got != "x «redacted-vault-secret» y «redacted-vault-secret»" {
		t.Fatalf("raw and normalized forms not scrubbed: %q", got)
	}
	if got := redactRegisteredVaultValues(envB, "hunter2pass"); got != "hunter2pass" {
		t.Fatalf("profile B scrubbed with profile A values: %q", got)
	}
	RegisterVaultRedactionValueIn(ctxA, "ab")
	RegisterVaultRedactionValueIn(ctxA, "abc")
	if got := redactRegisteredVaultValues(envA, "abc"); got != "«redacted-vault-secret»" {
		t.Fatalf("longest value not scrubbed first: %q", got)
	}
	ClearVaultRedactionValues(ctxA)
	for i := 0; i < vaultRedactionMaxPerProfile+1; i++ {
		RegisterVaultRedactionValueIn(ctxA, "v"+strings.Repeat("x", i)+"!")
	}
	RegisterVaultRedactionValueIn(ctxA, "v"+strings.Repeat("x", 1)+"!")
	vaultRedactionLock.Lock()
	bucket := append([]string{}, vaultRedactionValues[vaultScope(ctxA)]...)
	vaultRedactionLock.Unlock()
	if len(bucket) != vaultRedactionMaxPerProfile || bucket[0] != "v"+strings.Repeat("x", 2)+"!" || bucket[len(bucket)-1] != "vx!" {
		t.Fatalf("bucket eviction or recency wrong: len %d first %q last %q", len(bucket), bucket[0], bucket[len(bucket)-1])
	}
	RegisterVaultRedactionValueIn(ctxB, "bsecret")
	ClearVaultRedactionValues(ctxA)
	if got := redactRegisteredVaultValues(envB, "bsecret"); got != "«redacted-vault-secret»" {
		t.Fatalf("clearing profile A dropped profile B: %q", got)
	}
}

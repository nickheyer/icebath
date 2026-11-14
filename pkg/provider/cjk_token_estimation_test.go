package provider

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestMessageEstimateCountsKoreanContentAsTokenDense(t *testing.T) {
	messages := []*state.Message{{Role: "user", Content: "압축 테스트 " + strings.Repeat("가", 1000)}}
	if got := EstimateMessagesTokens(messages, DefaultImageTokenCost, true); got < 1000 {
		t.Fatalf("estimate %d", got)
	}
}

// Per character reference, CJK one token each and everything else UTF-8 bytes over four
func referencePerCharEstimate(text string) int {
	dense, sparseBytes := 0, 0
	for _, ch := range text {
		if cjkDenseRe.MatchString(string(ch)) {
			dense++
		} else {
			sparseBytes += utf8.RuneLen(ch)
		}
	}
	return dense + (sparseBytes+3)/4
}

func TestPerfGatedEstimatorMatchesPerCharReference(t *testing.T) {
	samples := []string{
		"", "ab", strings.Repeat("a", 400), strings.Repeat("가", 400), "압축 테스트 " + strings.Repeat("가", 1000),
		"café résumé naïve", "hello 안녕 world", "ｱｲｳｴｵ ﾃｽﾄ", "漢字とかな交じり文です。", "русский текст",
	}
	for _, text := range samples {
		if got, want := EstimateTokens(text), referencePerCharEstimate(text); got != want {
			t.Fatalf("%q: %d want %d", text, got, want)
		}
	}
}

func TestCyrillicCountsByUtf8Bytes(t *testing.T) {
	if got := EstimateTokens("русский текст"); got != 7 {
		t.Fatalf("cyrillic estimate %d", got)
	}
	if got := EstimateTokens(strings.Repeat("a", 400)); got != 100 {
		t.Fatalf("ascii estimate %d", got)
	}
}

func TestAccentedLatinIsNotInflatedByByteCounting(t *testing.T) {
	fr := "La compression du contexte permet aux longues sessions de rester dans la fenêtre du fournisseur sans perdre le fil de la tâche."
	asciiRule := (utf8.RuneCountInString(fr) + 3) / 4
	est := EstimateTokens(fr)
	if est < asciiRule || est > int(float64(asciiRule)*1.10) {
		t.Fatalf("ascii rule %d estimate %d", asciiRule, est)
	}
}

func TestMixedCyrillicAndAsciiCodeCountsAsciiAtOneByte(t *testing.T) {
	code := "def compress(ctx):\n    # Сжимаем контекст\n    return summarize(ctx)\n"
	asciiPart := "def compress(ctx):\n    # \n    return summarize(ctx)\n"
	cyr := "Сжимаем контекст"
	expected := (len(asciiPart) + len(cyr) + 3) / 4
	if got := EstimateTokens(code); got != expected {
		t.Fatalf("estimate %d want %d", got, expected)
	}
	if EstimateTokens(code) <= (utf8.RuneCountInString(code)+3)/4 {
		t.Fatal("byte counting must exceed the chars over four rule for Cyrillic")
	}
}

func TestLoneSurrogatesDoNotRaise(t *testing.T) {
	if EstimateTokens("abc\xed\xa0\x80def") < 2 {
		t.Fatal("lone high surrogate estimate too small")
	}
	if EstimateTokens("漢字\xed\xbf\xbf") < 2 {
		t.Fatal("lone low surrogate estimate too small")
	}
}

func TestIsLocalEndpointClassicLocalAddresses(t *testing.T) {
	for _, url := range []string{
		"http://localhost:11434", "http://127.0.0.1:8080", "http://0.0.0.0:5000", "http://[::1]:11434",
		"http://192.168.1.100:8000", "http://10.0.0.5:1234", "http://172.17.0.1:11434",
		"http://host.docker.internal:11434", "http://host.containers.internal:11434", "http://host.lima.internal:11434",
	} {
		if !IsLocalEndpoint(url) {
			t.Fatalf("%s read as remote", url)
		}
	}
}

func TestIsLocalEndpointRemoteEndpoints(t *testing.T) {
	for _, url := range []string{"https://api.openai.com", "https://openrouter.ai/api", "https://api.anthropic.com", "https://evil.docker.internal.example.com"} {
		if IsLocalEndpoint(url) {
			t.Fatalf("%s read as local", url)
		}
	}
}

func TestIsLocalEndpointMdnsAndIpv6Scope(t *testing.T) {
	for url, local := range map[string]bool{"http://byron.local:11434/v1": true, "http://[fd00::1]:11434": true, "http://[2607:f8b0::1]:11434": false} {
		if IsLocalEndpoint(url) != local {
			t.Fatalf("%s local=%t expected %t", url, !local, local)
		}
	}
}

func TestHyphenatedRelaySlugResolvesToSpecificCatalogEntry(t *testing.T) {
	ctx := contextWithTempHome(t)
	defaults := DefaultContextLengths()
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "z-ai-glm-5-3", Provider: "custom"}); got != defaults["glm-5.3"] {
		t.Fatalf("z-ai-glm-5-3 resolved %d want %d", got, defaults["glm-5.3"])
	}
	if defaults["glm-5.3"] <= defaults["glm"] {
		t.Fatal("the specific entry must beat the family catch all")
	}
}

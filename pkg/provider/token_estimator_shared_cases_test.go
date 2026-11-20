package provider

import (
	"strings"
	"testing"
	"unicode/utf8"
)

var (
	estimatorCyrillic = strings.Repeat("Привет мир, это проверка оценки токенов. ", 40)
	estimatorCJK      = strings.Repeat("これは日本語のテキストです。", 40)
)

// test_token_estimator_shared.py test_breakdown_and_retention_use_the_canonical_estimator
func TestBreakdownAndRetentionUseTheCanonicalEstimator(t *testing.T) {
	for _, text := range []string{estimatorCyrillic, estimatorCJK, strings.Repeat("plain ascii text ", 40)} {
		canonical := EstimateTokens(text)
		if got := approxTokens(text); got != canonical {
			t.Fatalf("retention estimate %d, canonical %d", got, canonical)
		}
	}
	cyrillicChars := utf8.RuneCountInString(estimatorCyrillic)
	if got := EstimateTokens(estimatorCyrillic); float64(got) <= float64((cyrillicChars+3)/4)*1.5 {
		t.Fatalf("cyrillic estimate %d under counts %d chars", got, cyrillicChars)
	}
	if got := EstimateTokens(estimatorCJK); got < utf8.RuneCountInString(estimatorCJK) {
		t.Fatalf("cjk estimate %d below its %d chars", got, utf8.RuneCountInString(estimatorCJK))
	}
}

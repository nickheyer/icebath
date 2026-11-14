package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestJiterStreamParseValueerrorIsNotLocal(t *testing.T) {
	for _, msg := range []string{
		"key must be a string at line 1 column 2",
		"EOF while parsing a value at line 1 column 5",
		"trailing characters at line 1 column 9",
		"expected `,` or `}` at line 1 column 8",
		"invalid number at line 1 column 7",
	} {
		if isLocalValidationError(&provider.LocalValidationError{Message: msg}) {
			t.Errorf("%q classified as a local bug", msg)
		}
	}
}

func TestUnrelatedValueerrorWithLineSuffixStaysLocal(t *testing.T) {
	if !isLocalValidationError(&provider.LocalValidationError{Message: "bad config at line 3 column 1"}) {
		t.Fatal("unrelated ValueError must stay local")
	}
}

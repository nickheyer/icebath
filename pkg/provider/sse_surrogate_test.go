package provider

import (
	"strings"
	"testing"
)

func TestNonJSONStreamTextDecodesEachInvalidByteAsPythonDoes(t *testing.T) {
	err := providerStreamErrorFromNonJSON("a\xed\xa0\x80b x\x80\x80y plain é", nil)
	if text := err.Error(); !strings.Contains(text, "a���b x��y plain é") {
		t.Fatalf("%q", text)
	}
}

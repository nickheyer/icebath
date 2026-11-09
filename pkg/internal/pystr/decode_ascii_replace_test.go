package pystr

import "testing"

func TestDecodeASCIIReplaceKeepsASCIIAndReplacesHighBytes(t *testing.T) {
	if got := DecodeASCIIReplace([]byte("git-commit\n")); got != "git-commit\n" {
		t.Fatalf("ascii %q", got)
	}
	if got := DecodeASCIIReplace([]byte{'g', 0xc3, 0xa9, 't'}); got != "g��t" {
		t.Fatalf("high bytes %q", got)
	}
	if got := DecodeASCIIReplace(nil); got != "" {
		t.Fatalf("empty %q", got)
	}
}

func TestDecodeUTF8SigIgnoreStripsTheBOMAndDropsInvalidBytes(t *testing.T) {
	if got := DecodeUTF8SigIgnore([]byte{0xEF, 0xBB, 0xBF, 'K', '=', 'v', 0xff, '\n'}); got != "K=v\n" {
		t.Fatalf("%q", got)
	}
	if got := DecodeUTF8SigIgnore([]byte("plain")); got != "plain" {
		t.Fatalf("%q", got)
	}
}

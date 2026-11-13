package dashboard

import (
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"testing"
)

// CSP source of the sha256 of a script text
func cspHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
}

func TestInlineScriptHashesFollowTheHTMLParserReading(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, tc := range []struct {
		html string
		want []string
	}{
		{`<script/>abc<b>x</b></script>`, []string{cspHash("")}},
		{`<script>a<!--b--></script>`, []string{cspHash("a<!--b-->")}},
		{`<script src="x.js"></script><script>boot()</script>`, []string{cspHash("boot()")}},
		{`<p>no scripts</p>`, nil},
	} {
		if got := inlineScriptHashes(tc.html); !slices.Equal(got, tc.want) {
			t.Fatalf("%s hashes %v want %v", tc.html, got, tc.want)
		}
	}
}

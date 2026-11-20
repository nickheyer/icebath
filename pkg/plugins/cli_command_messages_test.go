package plugins

import (
	"strings"
	"testing"
)

func TestPluginCloneFailureLeadsWithNextStepsAndEscapesGitOutput(t *testing.T) {
	msg := cloneFailureMessage("https://github.com/acme/nope", "fatal: repository '[x]' not found")
	if !strings.Contains(msg, `\[x]`) {
		t.Fatalf("msg = %q", msg)
	}
}

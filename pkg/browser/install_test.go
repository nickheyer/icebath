package browser

import "testing"

func TestRetiredAgentBrowserWarmerDoesNoWork(t *testing.T) {
	if WarmAgentBrowserNpxCache(60) || WarmAgentBrowserNpxCache(0.1) {
		t.Fatal("warmer reported work")
	}
}

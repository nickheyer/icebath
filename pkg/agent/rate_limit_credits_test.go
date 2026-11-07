package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestCreditsCaptureFollowsTheLiveRouteAndRoutesNotices(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var got []string
	a := &AIAgent{provider: "nous", model: "m1", baseURL: "https://inference-api.nousresearch.com/v1", sessionID: "s1",
		noticeCallback: func(n provider.AgentNotice) { got = append(got, n.Text) }}
	c := a.creditsCapture()
	if c.Provider != "nous" || c.Model != "m1" || c.SessionID != "s1" {
		t.Fatalf("capture route %s %s %s", c.Provider, c.Model, c.SessionID)
	}
	a.provider, a.model, a.baseURL, a.sessionID = "openrouter", "m2", "https://openrouter.ai/api/v1", "s2"
	if again := a.creditsCapture(); again != c || c.Provider != "openrouter" || c.Model != "m2" || c.SessionID != "s2" {
		t.Fatalf("capture not kept on the live route %s %s %s", c.Provider, c.Model, c.SessionID)
	}
	if c.Latch() == nil || !c.CreditsNoticesEnabled() {
		t.Fatal("fresh capture lacks the latch or the default on notices")
	}
	c.Notice(provider.AgentNotice{Text: "low credits"})
	if len(got) != 1 || got[0] != "low credits" {
		t.Fatalf("notice not routed %v", got)
	}
	if a.CreditsCapture() != c {
		t.Fatal("exported accessor returned another capture")
	}
}

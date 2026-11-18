package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/setupflow"
)

// Asker answering every choice with one index and recording what it said
type choiceAsker struct {
	index int
	said  []string
	asked []string
}

func (a *choiceAsker) Say(line string) { a.said = append(a.said, line) }
func (a *choiceAsker) Choose(question string, _ []setupflow.Option, _ int) (int, error) {
	a.asked = append(a.asked, question)
	return a.index, nil
}
func (a *choiceAsker) Check(string, []setupflow.Item, map[int]bool, map[string]int) (map[int]bool, error) {
	return nil, errors.New("no checklist here")
}
func (a *choiceAsker) Text(string, string, bool) (string, error) {
	return "", errors.New("no prompt here")
}

func TestSharedMetricsSectionConsentWritesBothKeys(t *testing.T) {
	authTestHome(t)
	cfg := map[string]any{}
	ask := &choiceAsker{index: 0}
	if err := SharedMetricsSectionConsent(context.Background(), cfg, ask); err != nil {
		t.Fatal(err)
	}
	shared := cfg["telemetry"].(map[string]any)["shared_metrics"].(map[string]any)
	if shared["enabled"] != true || shared["send"] != true || !strings.Contains(strings.Join(ask.said, "\n"), "sent to Nous once a day") {
		t.Fatalf("cfg %v said %v", cfg, ask.said)
	}
	ask = &choiceAsker{index: 1}
	if err := SharedMetricsSectionConsent(context.Background(), cfg, ask); err != nil {
		t.Fatal(err)
	}
	if shared := cfg["telemetry"].(map[string]any)["shared_metrics"].(map[string]any); shared["enabled"] != false || shared["send"] != false {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestNousPortalSectionAccessAsksBeforeTheLogin(t *testing.T) {
	_, env := authTestHome(t)
	prevInfo, prevLogin := nousSubAccountInfoFn, nousSectionLoginPersist
	t.Cleanup(func() { nousSubAccountInfoFn, nousSectionLoginPersist = prevInfo, prevLogin })
	paid := true
	signedIn := false
	nousSubAccountInfoFn = func(PoolEnv, bool) *NousPortalAccountInfo {
		if !signedIn {
			return &NousPortalAccountInfo{LoggedIn: false}
		}
		return &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid}
	}
	logins := 0
	nousSectionLoginPersist = func(PoolEnv, io.Writer) ([]map[string]any, error) {
		logins++
		signedIn = true
		return nil, nil
	}
	var out strings.Builder
	declined := func(string) (bool, error) { return false, nil }
	if NousPortalSectionAccess(env, &out, "Nous Subscription", "", declined) || logins != 0 || !strings.Contains(out.String(), "Skipped Nous Portal login.") {
		t.Fatalf("declined login output %q logins %d", out.String(), logins)
	}
	out.Reset()
	accepted := func(string) (bool, error) { return true, nil }
	if !NousPortalSectionAccess(env, &out, "Nous Subscription", "", accepted) || logins != 1 || !strings.Contains(out.String(), "Nous Portal login successful.") {
		t.Fatalf("accepted login output %q logins %d", out.String(), logins)
	}
}

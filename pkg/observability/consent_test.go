package observability

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
)

var realPurgePendingUpdates = purgePendingUpdates

// Points the profile at a temp home with terminal streams and the prompt replaced by ask
func consentTerminal(t *testing.T, ask func(title string, items []cliui.RadioItem, opts cliui.RadiolistOptions) int) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	mustMkdir(t, h)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_DESKTOP", "")
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	os.Unsetenv("ICEBATH_DESKTOP")
	os.Unsetenv("ICEBATH_NONINTERACTIVE")
	savedConsole, savedFlush, savedAsk, savedManaged := consentConsole, consentFlushStdin, consentRadiolist, consentIsManaged
	t.Cleanup(func() {
		consentConsole, consentFlushStdin, consentRadiolist, consentIsManaged = savedConsole, savedFlush, savedAsk, savedManaged
	})
	console := &cliui.Console{In: strings.NewReader(""), Out: &strings.Builder{},
		StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return true }}
	consentConsole = func() *cliui.Console { return console }
	consentFlushStdin = func() {}
	consentIsManaged = func(context.Context, string) bool { return false }
	consentRadiolist = func(_ context.Context, title string, items []cliui.RadioItem, _ int, opts cliui.RadiolistOptions) (int, error) {
		return ask(title, items, opts), nil
	}
	return h
}

// Reads the consent state of the temp profile's raw config.yaml
func consentOnDisk(t *testing.T, h string) ConsentAnswer {
	t.Helper()
	raw, err := config.ReadRawConfig(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	return ConsentState(raw)
}

func TestEveryOptOutAnswerPurgesPendingReceipts(t *testing.T) {
	m := installMarks(t, "{enabled: true}")
	t.Setenv("ICEBATH_HOME", m.home)
	savedManaged, savedRecord := consentIsManaged, recordSendConsentChange
	t.Cleanup(func() { consentIsManaged, recordSendConsentChange = savedManaged, savedRecord })
	consentIsManaged = func(context.Context, string) bool { return false }
	recordSendConsentChange = func(context.Context, bool) {}
	purgePendingUpdates = realPurgePendingUpdates
	install := parkReceipt(t, m.home, installReceipt(nil))
	parked := PendingUpdatesDir(m.home)
	mustMkdir(t, parked)
	if err := os.WriteFile(filepath.Join(parked, strings.Repeat("f", 32)+".json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := SaveConsent(ctx, true, false, nil); err != nil {
		t.Fatal(err)
	}
	if !pathExists(install) || !pathExists(parked) {
		t.Fatal("an opt in purged the parked receipts")
	}
	if err := SaveConsent(ctx, false, false, nil); err != nil {
		t.Fatal(err)
	}
	if pathExists(install) || pathExists(parked) {
		t.Fatal("an opt out kept the parked receipts")
	}
}

// Reports whether a path exists on disk
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestChatOfferAsksAnUndecidedProfileOnce(t *testing.T) {
	var asked [][]string
	h := consentTerminal(t, func(_ string, items []cliui.RadioItem, _ cliui.RadiolistOptions) int {
		labels := make([]string, len(items))
		for i, item := range items {
			labels[i] = cliui.RadioItemPlain(item)
		}
		asked = append(asked, labels)
		return 0
	})
	ctx := context.Background()
	OfferConsentBeforeChat(ctx, ConsentChatArgs{Query: "hi"})
	if len(asked) != 0 {
		t.Fatal("a query run was asked")
	}
	OfferConsentBeforeChat(ctx, ConsentChatArgs{})
	OfferConsentBeforeChat(ctx, ConsentChatArgs{})
	if len(asked) != 1 || !reflect.DeepEqual(asked[0], []string{"Share with Nous", "No thanks"}) {
		t.Fatalf("asked %v", asked)
	}
	if got := consentOnDisk(t, h); got != (ConsentAnswer{Enabled: true, Send: true, Decided: true}) {
		t.Fatalf("state %+v", got)
	}
}

func TestOfferDropsKeysTypedBeforeItAppeared(t *testing.T) {
	var events []string
	consentTerminal(t, func(string, []cliui.RadioItem, cliui.RadiolistOptions) int {
		events = append(events, "ask")
		return -1
	})
	consentFlushStdin = func() { events = append(events, "flush") }
	if _, err := OfferConsent(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"flush", "ask"}) {
		t.Fatalf("events %v", events)
	}
}

func TestUnseenNoThanksIsAskedOnceMoreAndThenSettled(t *testing.T) {
	var shown []string
	h := consentTerminal(t, func(_ string, _ []cliui.RadioItem, opts cliui.RadiolistOptions) int {
		shown = append(shown, opts.Description)
		return -1
	})
	ctx := context.Background()
	for _, send := range []string{"true", "false"} {
		writeSharedMetricsConfig(t, h, "{enabled: true, send: "+send+"}")
		OfferConsentBeforeChat(ctx, ConsentChatArgs{})
	}
	if len(shown) != 0 {
		t.Fatalf("an opt in was re-asked %v", shown)
	}
	writeSharedMetricsConfig(t, h, "{enabled: false, send: false}")
	OfferConsentBeforeChat(ctx, ConsentChatArgs{})
	OfferConsentBeforeChat(ctx, ConsentChatArgs{})
	if len(shown) != 1 || !strings.HasPrefix(shown[0], reaskNote) {
		t.Fatalf("shown %v", shown)
	}
	if got := consentOnDisk(t, h); got != (ConsentAnswer{Decided: true}) {
		t.Fatalf("state %+v", got)
	}
}

func TestOfferDescriptionNamesTheInstallNoteBeforeThePreOptInPromise(t *testing.T) {
	text := strings.Join(strings.Fields(offerDescription), " ")
	before, _, found := strings.Cut(text, "before you opt in")
	if !found || !strings.Contains(before, "install note") {
		t.Fatal(text)
	}
}

func TestConsentStateReadsRawConfigOnly(t *testing.T) {
	cases := []struct {
		raw  any
		want ConsentAnswer
	}{
		{nil, ConsentAnswer{}},
		{map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true}}}, ConsentAnswer{Enabled: true, Decided: true}},
		{map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"send": true}}}, ConsentAnswer{Reask: true}},
		{map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": false, "offer_version": 2}}}, ConsentAnswer{Decided: true}},
		{map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": false, "offer_version": 2.0}}}, ConsentAnswer{Decided: true}},
		{map[string]any{"telemetry": "x"}, ConsentAnswer{}},
	}
	for _, c := range cases {
		if got := ConsentState(c.raw); got != c.want {
			t.Errorf("%v: %+v want %+v", c.raw, got, c.want)
		}
	}
	target := map[string]any{"telemetry": "x"}
	SetAnswer(target, false, true)
	if !reflect.DeepEqual(target, map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": false, "send": false, "offer_version": 2}}}) {
		t.Fatal(target)
	}
}

func TestOfferDescriptionIsTheOfferExplainer(t *testing.T) {
	if OfferDescription() != offerDescription || !strings.Contains(OfferDescription(), "Change it any time: icebath setup telemetry") {
		t.Fatalf("description %q", OfferDescription())
	}
}

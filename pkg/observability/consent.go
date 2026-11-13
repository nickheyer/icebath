package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	term "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// OfferChoice is one answer of the consent offer with the two opt ins it writes
type OfferChoice struct {
	Label   string
	Enabled bool
	Send    bool
}

// OfferChoices lists the offer's answers in the order every surface shows them
var OfferChoices = []OfferChoice{{"Share with Nous", true, true}, {"No thanks", false, false}}

var noThanks = len(OfferChoices) - 1

const (
	// DocsURL is the developer guide page describing shared metrics
	DocsURL = "https://hermes-agent.nousresearch.com/docs/developer-guide/relay-shared-metrics"
	// OfferVersion is bumped only when every earlier answer must be asked again
	OfferVersion = 2
	reaskNote    = "Asking once more: an earlier version could save \"No thanks\" before you saw this question."
)

var offerDescription = strings.Join([]string{
	"Shared metrics are bounded counters: activity, outcomes, error classes (with a",
	"fixed-list reason when a memory write, compression, update or install fails),",
	"model routes, token totals, feature use and coarse machine facts. Never prompts,",
	"files, paths, setting values or error text. Sharing sends them to Nous once a",
	"day. Apart from a one-time install note, data from before you opt in is never",
	"sent.",
	"Details: " + DocsURL,
	"Change it any time: icebath setup telemetry",
}, "\n")

// OfferDescription is the explainer the consent offer shows under its question
func OfferDescription() string { return offerDescription }

// ConsentAnswer is the consent state read from a raw config mapping
type ConsentAnswer struct {
	Enabled bool
	Send    bool
	Decided bool
	Reask   bool
}

// ConsentChatArgs carries the chat command flags that rule out a person at a terminal
type ConsentChatArgs struct {
	Query            string
	QueryFile        string
	Oneshot          string
	OneshotExit      bool
	SafeMode         bool
	IgnoreUserConfig bool
}

var (
	consentIsManaged        = func(ctx context.Context, h string) bool { return config.ManagedSystem(ctx, h) != "" }
	recordSendConsentChange = RecordSendConsentChange
	consentConsole          = cliui.Default
	consentFlushStdin       = func() { cliui.Default().FlushStdin() }
	consentRadiolist        = func(ctx context.Context, title string, items []cliui.RadioItem, selected int, opts cliui.RadiolistOptions) (int, error) {
		return cliui.Default().CursesRadiolist(ctx, title, items, selected, opts)
	}
	consentStdout = func() *os.File { return os.Stdout }
)

// Returns the telemetry.shared_metrics mapping of a config, empty when any level is not a mapping
func section(cfg any) map[string]any {
	m, _ := cfg.(map[string]any)
	telemetry, _ := m["telemetry"].(map[string]any)
	s, _ := telemetry["shared_metrics"].(map[string]any)
	if s == nil {
		return map[string]any{}
	}
	return s
}

// Reads enabled, send, decided and reask from a raw config with an old off answer reading undecided
func ConsentState(rawCfg any) ConsentAnswer {
	s := section(rawCfg)
	enabled := s["enabled"] == true
	_, hasEnabled := s["enabled"]
	_, hasSend := s["send"]
	answered := hasEnabled || hasSend
	reask := answered && !enabled && !isOfferVersion(s["offer_version"])
	return ConsentAnswer{Enabled: enabled, Send: enabled && s["send"] == true, Decided: answered && !reask, Reask: reask}
}

// Reports whether a stored offer_version equals the current version under Python equality
func isOfferVersion(v any) bool {
	switch n := v.(type) {
	case int:
		return n == OfferVersion
	case int64:
		return n == OfferVersion
	case uint64:
		return n == OfferVersion
	case float64:
		return n == OfferVersion
	case bool:
		return n && OfferVersion == 1 || !n && OfferVersion == 0
	}
	return false
}

// Reads the consent state of the active profile's raw config.yaml
func currentState(ctx context.Context) (ConsentAnswer, error) {
	raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		return ConsentAnswer{}, err
	}
	return ConsentState(raw), nil
}

// Writes both opt ins and the settling offer version into a config mapping
func SetAnswer(target map[string]any, enabled, send bool) {
	telemetry, ok := target["telemetry"].(map[string]any)
	if !ok {
		telemetry = map[string]any{}
		target["telemetry"] = telemetry
	}
	s, ok := telemetry["shared_metrics"].(map[string]any)
	if !ok {
		s = map[string]any{}
		telemetry["shared_metrics"] = s
	}
	s["enabled"], s["send"] = enabled, enabled && send
	s["offer_version"] = OfferVersion
}

// Writes the answer to the raw config.yaml, reconciles send windows and purges receipts on an opt out
func SaveConsent(ctx context.Context, enabled, send bool, cfg map[string]any) error {
	h := home.Dir(ctx)
	if consentIsManaged(ctx, h) {
		config.ManagedError(ctx, h, "save configuration")
		return nil
	}
	send = enabled && send
	configPath := home.ConfigPath(h)
	userConfig, err := config.RequireReadableConfigBeforeWrite(ctx, configPath)
	if err != nil {
		return err
	}
	SetAnswer(userConfig, enabled, send)
	if err := config.WriteUserConfig(ctx, h, configPath, userConfig); err != nil {
		return err
	}
	if cfg != nil {
		SetAnswer(cfg, enabled, send)
	}
	recordSendConsentChange(ctx, send)
	if !enabled {
		PurgePendingReceipts(ctx, h)
	}
	return nil
}

// Reconciles shared metrics consent windows the moment the user decides, never failing the caller
func RecordSendConsentChange(ctx context.Context, enabled bool) {
	store, err := NewSharedMetricsStore(ctx, "", "")
	if err == nil {
		err = store.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
			return ReconcileSendConsent(ctx, conn, enabled, time.Now())
		})
	}
	if err != nil {
		slog.Debug("Unable to record shared-metrics consent change", "error", err)
	}
}

// Asks the consent question once with No thanks preselected, true when the question got an answer
func OfferConsent(ctx context.Context, cfg map[string]any, reask bool) (bool, error) {
	c := consentConsole()
	consentFlushStdin()
	labels := make([]cliui.RadioItem, len(OfferChoices))
	for i, choice := range OfferChoices {
		labels[i] = cliui.PlainItem(choice.Label)
	}
	description := offerDescription
	if reask {
		description = reaskNote + "\n" + offerDescription
	}
	cancel := -1
	idx, err := consentRadiolist(ctx, "Help improve Icebath?", labels, noThanks,
		cliui.RadiolistOptions{CancelReturns: &cancel, Description: description})
	if err != nil {
		return false, err
	}
	if idx < 0 && reask {
		if err := SaveConsent(ctx, false, false, cfg); err != nil {
			return false, err
		}
		c.PrintInfo("Kept \"No thanks\". Change it any time with `icebath setup telemetry`.")
		return true, nil
	}
	if idx < 0 {
		c.PrintInfo("Not answered; Icebath will ask again. Decide any time with `icebath setup telemetry`.")
		return false, nil
	}
	choice := OfferChoices[idx]
	if err := SaveConsent(ctx, choice.Enabled, choice.Send, cfg); err != nil {
		return false, err
	}
	outcome := "off"
	if choice.Send {
		outcome = "sent to Nous once a day"
	}
	c.PrintSuccess(fmt.Sprintf("Shared metrics %s. Change it any time with `icebath setup telemetry`.", outcome))
	return true, nil
}

// Reports whether no human can answer a prompt, as dashboard spawned actions declare
func setupIsNoninteractive() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_NONINTERACTIVE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Reports whether a console stream is a terminal through its override check or isatty
func streamIsTerminal(check func() bool, stream any) bool {
	if check != nil {
		return check()
	}
	f, ok := stream.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// Reports whether an error is the Ctrl+C or end of input that ends a prompt
func promptAborted(err error) bool {
	return errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF)
}

// Offers consent at the end of every setup flow to a terminal user whose profile never answered
func OfferConsentIfUndecided(ctx context.Context, cfg map[string]any) error {
	if consentIsManaged(ctx, home.Dir(ctx)) || setupIsNoninteractive() || !streamIsTerminal(consentConsole().StdinTTY, consentConsole().In) {
		return nil
	}
	st, err := currentState(ctx)
	if err != nil {
		return err
	}
	if st.Decided {
		return nil
	}
	if _, err := OfferConsent(ctx, cfg, st.Reask); err != nil {
		if promptAborted(err) {
			fmt.Fprintln(consentStdout())
			return nil
		}
		return err
	}
	return nil
}

// Offers consent once before an interactive chat with a person at a terminal on an undecided profile
func OfferConsentBeforeChat(ctx context.Context, args ConsentChatArgs) {
	if args.Query != "" || args.QueryFile != "" || args.Oneshot != "" || args.OneshotExit {
		return
	}
	if args.SafeMode || args.IgnoreUserConfig {
		return
	}
	if os.Getenv("ICEBATH_DESKTOP") != "" || setupIsNoninteractive() {
		return
	}
	c := consentConsole()
	if !(streamIsTerminal(c.StdinTTY, c.In) && streamIsTerminal(c.StdoutTTY, c.Out)) || consentIsManaged(ctx, home.Dir(ctx)) {
		return
	}
	st, err := currentState(ctx)
	if err == nil {
		if st.Decided {
			return
		}
		_, err = OfferConsent(ctx, nil, st.Reask)
	}
	if err == nil {
		return
	}
	if promptAborted(err) {
		fmt.Fprintln(consentStdout())
		return
	}
	slog.Debug("shared-metrics offer skipped", "error", err)
}

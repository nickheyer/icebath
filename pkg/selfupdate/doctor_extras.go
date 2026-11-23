//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Adds the Helper Programs and Optional Python Features sections of the package manager to the walk
func init() {
	doctorTierChecks = append(doctorTierChecks,
		doctorCheckEntry{"Helper Programs", checkHelperPrograms}, doctorCheckEntry{"Optional Python Features", checkPythonExtras})
}

// Installed helper programs and Python environment this version's pins have moved past, replaced by tests
var doctorOutdatedPrograms = pm.Outdated

// Reports each installed helper program behind this version's pins, bringing it current with fix, and a Python environment behind its lock
func checkHelperPrograms(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Helper program check failed: {e}", "", func(f *Finding) error {
		report, err := doctorOutdatedPrograms(ctx)
		if err != nil {
			return err
		}
		if len(report.Names) == 0 {
			doctorCheckOK(d.W, "Installed helper programs match this version", "")
			return nil
		}
		for _, name := range report.Names {
			if name == "venv" {
				doctorCheckWarn(d.W, "The Python environment is behind this version", "("+report.Reasons[name]+"; run: icebath pm install)")
				f.ManualIssues = append(f.ManualIssues, "Run `icebath pm install` to bring the Python environment to this version.")
				continue
			}
			command := "icebath pm install " + name
			if !shouldFix {
				doctorCheckWarn(d.W, name+" is behind this version", "(run: "+command+")")
				f.Issues = append(f.Issues, "Run `"+command+"` to bring "+name+" to this version, or `icebath doctor --fix`.")
				continue
			}
			if err := doctorPMEnsure(ctx, name); err != nil {
				if !doctorPMErrorReported(err) {
					return err
				}
				doctorCheckWarn(d.W, name+" could not be updated", "("+err.Error()+")")
				f.ManualIssues = append(f.ManualIssues, "Run `"+command+"` once the cause is fixed.")
				continue
			}
			doctorCheckOK(d.W, name+" updated to this version", "")
			f.Fixed++
		}
		return nil
	})
}

// One switched on feature of the user's config and the extra it runs on
type extraDemand struct {
	feature string
	extra   string
}

// Features the config switches on that run on an extra, the Relay plugins left to their own check
func switchedOnExtras(ctx context.Context, h string) []extraDemand {
	var out []extraDemand
	sctx := home.WithOverride(ctx, h)
	if settings.RecognitionService.Get(sctx) == "local" {
		out = append(out, extraDemand{"Local Whisper speech to text (stt.provider local)", "stt-whisper"})
	}
	switch provider := settings.SpeechVoice.Get(sctx); provider {
	case "neutts", "kittentts":
		out = append(out, extraDemand{"text to speech (tts.provider " + provider + ")", provider})
	}
	if settings.WakeWord.Get(sctx) {
		switch provider := strings.ToLower(strings.TrimSpace(settings.WakeWordProvider.Get(sctx))); provider {
		case "openwakeword", "oww", "local":
			out = append(out, extraDemand{"wake word (wake_word.provider " + provider + ")", "wake-openwakeword"})
		case "porcupine":
			out = append(out, extraDemand{"wake word (wake_word.provider porcupine)", "wake-porcupine"})
		}
	}
	if gw, err := gateway.LoadGatewayConfig(ctx); err == nil {
		for _, platform := range gw.GetConnectedPlatforms(ctx) {
			switch name := string(platform); name {
			case "qqbot", "weixin":
				out = append(out, extraDemand{"voice messages of the " + name + " platform (SILK audio)", "silk"})
			}
		}
	}
	return out
}

// Lists provisioned extras and warns for each switched on feature whose extra is missing or behind its pins
func checkPythonExtras(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Optional Python feature check failed: {e}", "", func(f *Finding) error {
		states, err := pm.ExtraStates(ctx)
		if err != nil {
			return err
		}
		byName := map[string]pm.ExtraState{}
		for _, state := range states {
			byName[state.Name] = state
			switch {
			case state.Provisioned && state.Current:
				doctorCheckOK(d.W, "Extra "+state.Name+" provisioned", "("+state.Feature+")")
			case state.Provisioned:
				doctorCheckWarn(d.W, "Extra "+state.Name+" was provisioned with older pins", "(run: "+pm.InstallHint(state.Name)+")")
				f.ManualIssues = append(f.ManualIssues, "Run `"+pm.InstallHint(state.Name)+"` to bring "+state.Name+" to this version's pins.")
			}
		}
		demands := switchedOnExtras(ctx, d.Home)
		for _, demand := range demands {
			state := byName[demand.extra]
			switch {
			case state.Unsupported != "":
				doctorCheckWarn(d.W, "The "+demand.feature+" is switched on but cannot run on this platform", "("+state.Unsupported+")")
			case !state.Provisioned:
				doctorCheckWarn(d.W, "The "+demand.feature+" is switched on but its "+demand.extra+" extra is not provisioned", "(run: "+pm.InstallHint(demand.extra)+")")
				f.ManualIssues = append(f.ManualIssues, "Run `"+pm.InstallHint(demand.extra)+"` for the "+demand.feature+".")
			}
		}
		if len(demands) == 0 && !anyProvisioned(states) {
			doctorCheckOK(d.W, "No optional Python feature is switched on", "")
		}
		return nil
	})
}

// Whether any extra is provisioned on this install
func anyProvisioned(states []pm.ExtraState) bool {
	for _, state := range states {
		if state.Provisioned {
			return true
		}
	}
	return false
}

// Brings every provisioned extra whose pins this version changed to the new pins with progress, absent extras staying absent
func refreshProvisionedExtras(ctx context.Context, w io.Writer) error {
	states, err := pm.ExtraStates(ctx)
	if err != nil {
		return err
	}
	for _, state := range states {
		if !state.Provisioned || state.Current || state.Name == "nemo-relay" || state.Unsupported != "" {
			continue
		}
		fmt.Fprintf(w, "\n→ Updating the %s extra to this version's pins...\n", state.Name)
		if _, err := pm.EnsureExtra(ctx, state.Name, pm.ExtraOptions{Output: w}); err != nil {
			fmt.Fprintf(w, "  ⚠ The %s extra keeps the environment it had, which still works; its update failed: %s\n", state.Name, err)
			var install *pm.InstallError
			if !errors.As(err, &install) {
				fmt.Fprintf(w, "    Run `%s` once the cause is fixed\n", pm.InstallHint(state.Name))
			}
			continue
		}
		fmt.Fprintf(w, "✓ %s\n", state.Name)
	}
	return nil
}

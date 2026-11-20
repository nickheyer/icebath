package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Guard reason of a host with a single profile, which has nothing to multiplex
const SingleProfileReason = "only one profile exists (nothing to multiplex)"

// One time marker a gateway leaves after rewriting a retired multiplex opt out
const RewrittenMarkerName = ".multiplex_opt_out_rewritten"

// Boxed one time notice of a rewritten multiplex opt out
func RetiredOptOutNoticeLines() []string {
	return multiplexBox([]string{"⚠ gateway.multiplex_profiles: false is retired and was rewritten to true;",
		"one gateway per host serves every profile.",
		"A per-profile gateway is `gateway.standalone: true` in that profile's config",
		"(temporary shim) or `--force`."})
}

// The rewrite notice once more for the update summary, clearing its marker, empty when none is pending
func ConsumeRewrittenNotice(defaultHome string) []string {
	if defaultHome == "" {
		defaultHome = home.Root()
	}
	marker := filepath.Join(defaultHome, RewrittenMarkerName)
	if _, err := os.Stat(marker); err != nil {
		return []string{}
	}
	if err := os.Remove(marker); err != nil {
		return []string{}
	}
	return RetiredOptOutNoticeLines()
}

// Lines framed in a single line box sized to the widest line
func multiplexBox(body []string) []string {
	width := 0
	for _, line := range body {
		width = max(width, utf8.RuneCountInString(line))
	}
	width += 2
	out := []string{"┌" + strings.Repeat("─", width) + "┐"}
	for _, line := range body {
		out = append(out, "│ "+line+strings.Repeat(" ", max(0, width-1-utf8.RuneCountInString(line)))+"│")
	}
	return append(out, "└"+strings.Repeat("─", width)+"┘")
}

// Named profiles a standalone gateway of this home leaves without a bot
func UnservedProfiles(ctx context.Context) ([]string, error) {
	me, ok := ProfileNameForHome(home.Dir(ctx))
	if !ok || me == "" {
		me = "default"
	}
	served, err := profiles.ProfilesToServe(ctx, true, false, true)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, p := range served {
		if p.Name != me {
			out = append(out, p.Name)
		}
	}
	return out, nil
}

// Boxed warning of a guard kept standalone gateway on a host with unserved profiles, empty otherwise
func StandaloneWarningLines(ctx context.Context, source, reason string, unserved []string, haveUnserved bool) []string {
	if source != "guard" || reason == SingleProfileReason {
		return []string{}
	}
	if !haveUnserved {
		var err error
		if unserved, err = UnservedProfiles(ctx); err != nil {
			unserved = nil
		}
	}
	if len(unserved) == 0 {
		return []string{}
	}
	return multiplexBox([]string{
		"⚠ This gateway is STANDALONE: it serves only its own profile.",
		"Profiles NOT served (their bots stay silent): " + strings.Join(unserved, ", "),
		"Why: " + reason,
		"Fix: " + HostMigrateCommand,
	})
}

// Standalone warning box rebuilt from the live gateway runtime status of this home
func RecordedStandaloneWarningLines(ctx context.Context) []string {
	reason := RecordedStandaloneReason("")
	if reason == "" {
		return []string{}
	}
	return StandaloneWarningLines(ctx, "guard", reason, nil, false)
}

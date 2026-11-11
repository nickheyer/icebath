package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One package of an advisory with its compromised versions, an empty set flagging any installed version
type AdvisoryPackage struct {
	Name     string
	Versions map[string]bool
}

// One security advisory, its stable id, the compromised packages and the ordered remediation steps
type Advisory struct {
	ID          string
	Title       string
	Summary     string
	URL         string
	Compromised []AdvisoryPackage
	Remediation []string
	Published   string
	Severity    string
}

// Advisory catalog, entries are appended and never removed
var Advisories = []Advisory{
	{
		ID:    "shai-hulud-2026-05",
		Title: "Mini Shai-Hulud worm — mistralai 2.4.6 compromised on PyPI",
		Summary: "PyPI quarantined the mistralai package on 2026-05-12 after a " +
			"malicious 2.4.6 release. The worm steals credentials from " +
			"environment variables and credential files (~/.npmrc, ~/.pypirc, " +
			"~/.aws/credentials, GitHub PATs, cloud SDK tokens) and exfils " +
			"them to a hardcoded webhook. If you ran any Python process that " +
			"imported mistralai 2.4.6 — including icebath when configured " +
			"with provider=mistral for TTS or STT — assume those credentials " +
			"are exposed. PyPI has since removed 2.4.6 and the project ships " +
			"clean releases again (2.4.7, 2.4.8); this advisory only fires if " +
			"the compromised 2.4.6 is still installed.",
		URL:         "https://socket.dev/blog/mini-shai-hulud-worm-pypi",
		Compromised: []AdvisoryPackage{{Name: "mistralai", Versions: map[string]bool{"2.4.6": true}}},
		Remediation: []string{
			"Run: pip uninstall -y mistralai  (or: uv pip uninstall mistralai)",
			"Rotate API keys in {icebath_home}/.env (OpenRouter, Anthropic, OpenAI, " +
				"Nous, GitHub, AWS, Google, Mistral, etc.).",
			"Audit ~/.npmrc, ~/.pypirc, ~/.aws/credentials, ~/.config/gh/hosts.yml, " +
				"and any other credential files for tokens that may have been read.",
			"Check GitHub for unexpected new SSH keys, deploy keys, or webhook " +
				"additions on repos you have admin on.",
			"After cleanup: icebath doctor --ack shai-hulud-2026-05  to dismiss " +
				"this warning.",
		},
		Published: "2026-05-12",
		Severity:  "critical",
	},
}

// One package version match against an advisory
type AdvisoryHit struct {
	Advisory         Advisory
	Package          string
	InstalledVersion string
}

// Installed version of a distribution on the process import path, false when absent or without a version header
func advisoryInstalledVersion(pkgName string) (string, bool) {
	version, found := findDistributionVersion(pkgName, processImportPath())
	if !found || version == "" {
		return "", false
	}
	return version, true
}

// Every advisory package that is installed at a compromised version, or at any version for an empty set
func DetectCompromised(advisories []Advisory) []AdvisoryHit {
	hits := []AdvisoryHit{}
	for _, advisory := range advisories {
		for _, pkg := range advisory.Compromised {
			installed, ok := advisoryInstalledVersion(pkg.Name)
			if ok && (len(pkg.Versions) == 0 || pkg.Versions[installed]) {
				hits = append(hits, AdvisoryHit{Advisory: advisory, Package: pkg.Name, InstalledVersion: installed})
			}
		}
	}
	return hits
}

// Advisory ids dismissed under security.acked_advisories, empty when the config does not load
func GetAckedAdvisoryIDs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		slog.Debug("Could not load config for advisory acks", "error", err)
		return out
	}
	security, ok := cfg["security"].(map[string]any)
	if cfg["security"] != nil && pyrepr.Truthy(cfg["security"]) && !ok {
		slog.Debug("Could not load config for advisory acks", "error", "security is not a mapping")
		return out
	}
	raw, _ := security["acked_advisories"].([]any)
	for _, x := range raw {
		if id := pystr.Strip(pyrepr.Str(x)); id != "" {
			out[id] = true
		}
	}
	return out
}

// Persists the dismissal of an advisory id in config.yaml, true when it is stored
func AckAdvisory(ctx context.Context, advisoryID string) bool {
	advisoryID = pystr.Strip(advisoryID)
	if advisoryID == "" {
		return false
	}
	fail := func(err error) bool {
		slog.Error(fmt.Sprintf("Failed to persist advisory ack for %s", advisoryID), "error", err)
		return false
	}
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return fail(err)
	}
	if _, has := cfg["security"]; !has {
		cfg["security"] = map[string]any{}
	}
	sec, ok := cfg["security"].(map[string]any)
	if !ok {
		return fail(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(cfg["security"])))
	}
	existing, _ := sec["acked_advisories"].([]any)
	for _, x := range existing {
		if pyrepr.Equal(x, advisoryID) {
			return true
		}
	}
	sec["acked_advisories"] = append(append([]any{}, existing...), advisoryID)
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return fail(err)
	}
	return true
}

// Hits whose advisories are not dismissed
func FilterUnackedAdvisories(ctx context.Context, hits []AdvisoryHit) []AdvisoryHit {
	out := []AdvisoryHit{}
	if len(hits) == 0 {
		return out
	}
	acked := GetAckedAdvisoryIDs(ctx)
	for _, h := range hits {
		if !acked[h.Advisory.ID] {
			out = append(out, h)
		}
	}
	return out
}

// Whether stdout is a terminal with NO_COLOR unset
func advisoryTermSupportsColor() bool {
	return os.Getenv("NO_COLOR") == "" && xterm.IsTerminal(os.Stdout.Fd())
}

// One to three unstyled startup banner lines naming the first hit and counting the others
func AdvisoryShortBannerLines(hits []AdvisoryHit) []string {
	if len(hits) == 0 {
		return []string{}
	}
	primary := hits[0]
	lines := []string{fmt.Sprintf("SECURITY ADVISORY [%s]: %s", primary.Advisory.ID, primary.Advisory.Title)}
	if len(hits) > 1 {
		suffix := "y"
		if len(hits) > 2 {
			suffix = "ies"
		}
		lines = append(lines, fmt.Sprintf("  (%d additional advisor%s also active.)", len(hits)-1, suffix))
	}
	return append(lines,
		fmt.Sprintf("  Detected: %s==%s", primary.Package, primary.InstalledVersion),
		"  Run 'icebath doctor' for remediation steps.")
}

// Applies str.format with the single keyword icebath_home, raising what format raises for any other field
func advisoryFormatStep(step, shownHome string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(step); i++ {
		switch c := step[i]; {
		case c == '{' && i+1 < len(step) && step[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(step) && step[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '}':
			return "", errors.New("Single '}' encountered in format string")
		case c == '{':
			end := strings.IndexByte(step[i+1:], '}')
			if end < 0 {
				return "", errors.New("expected '}' before end of string")
			}
			switch field := step[i+1 : i+1+end]; field {
			case "icebath_home":
				b.WriteString(shownHome)
			case "":
				return "", errors.New("Replacement index 0 out of range for positional args tuple")
			default:
				return "", fmt.Errorf("'%s'", field)
			}
			i += end + 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// Multi line block describing the advisory of a hit and its numbered remediation steps
func AdvisoryFullRemediationText(ctx context.Context, hit AdvisoryHit) ([]string, error) {
	a := hit.Advisory
	lines := []string{
		fmt.Sprintf("=== %s ===", a.Title),
		fmt.Sprintf("ID:        %s    Severity: %s    Published: %s", a.ID, a.Severity, a.Published),
		fmt.Sprintf("Detected:  %s==%s", hit.Package, hit.InstalledVersion),
		fmt.Sprintf("Reference: %s", a.URL),
		"",
		a.Summary,
		"",
		"Remediation:",
	}
	shown := home.Display(home.Dir(ctx))
	for i, step := range a.Remediation {
		text, err := advisoryFormatStep(step, shown)
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, text))
	}
	return lines, nil
}

const (
	advisoryBannerCacheFile   = "advisory_banner_seen"
	advisoryBannerRepeatHours = 24
)

// Banner cache file under the home cache directory, created on demand, false when it cannot be made
func advisoryBannerCachePath(ctx context.Context) (string, bool) {
	cacheDir := pypath.PathJoin(home.Dir(ctx), "cache")
	if err := pyfs.Makedirs(cacheDir, 0o777, true); err != nil {
		return "", false
	}
	return pypath.PathJoin(cacheDir, advisoryBannerCacheFile), true
}

// One advisory id of the banner cache with the epoch second its banner was last shown
type advisoryBannerSeen struct {
	id string
	ts float64
}

// Reads the banner cache in file order, a later line of an id replacing its time in place
func advisoryReadBannerCache(ctx context.Context) []advisoryBannerSeen {
	out := []advisoryBannerSeen{}
	p, ok := advisoryBannerCachePath(ctx)
	if !ok {
		return out
	}
	if _, err := os.Stat(p); err != nil {
		return out
	}
	data, err := os.ReadFile(p)
	if err != nil || !utf8.Valid(data) {
		return out
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	index := map[string]int{}
	for _, line := range pystr.Splitlines(text, false) {
		parts := pystr.SplitWhitespace(line, 1)
		if len(parts) != 2 {
			continue
		}
		ts, err := pyint.ParseFloat(parts[1])
		if err != nil {
			continue
		}
		if i, seen := index[parts[0]]; seen {
			out[i].ts = ts
			continue
		}
		index[parts[0]] = len(out)
		out = append(out, advisoryBannerSeen{id: parts[0], ts: ts})
	}
	return out
}

// Writes the banner cache as one id and time per line, a failure only logged
func advisoryWriteBannerCache(ctx context.Context, seen []advisoryBannerSeen) {
	p, ok := advisoryBannerCachePath(ctx)
	if !ok {
		return
	}
	lines := make([]string, 0, len(seen))
	for _, s := range seen {
		lines = append(lines, s.id+" "+pyrepr.Float(s.ts))
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o666); err != nil {
		slog.Debug("Could not write advisory banner cache", "error", err)
	}
}

// Hits not dismissed and not shown within the repeat window, each stamped into the banner cache
func AdvisoryHitsDueForBanner(ctx context.Context, hits []AdvisoryHit, repeatHours int) []AdvisoryHit {
	due := []AdvisoryHit{}
	fresh := FilterUnackedAdvisories(ctx, hits)
	if len(fresh) == 0 {
		return due
	}
	now := float64(time.Now().UnixNano()) / 1e9
	cache := advisoryReadBannerCache(ctx)
	cutoff := now - float64(repeatHours*3600)
	last := func(id string) (int, float64) {
		for i, s := range cache {
			if s.id == id {
				return i, s.ts
			}
		}
		return -1, 0.0
	}
	for _, hit := range fresh {
		if _, ts := last(hit.Advisory.ID); ts < cutoff {
			due = append(due, hit)
		}
	}
	for _, hit := range due {
		if i, _ := last(hit.Advisory.ID); i >= 0 {
			cache[i].ts = now
		} else {
			cache = append(cache, advisoryBannerSeen{id: hit.Advisory.ID, ts: now})
		}
	}
	if len(due) > 0 {
		advisoryWriteBannerCache(ctx, cache)
	}
	return due
}

// Printable startup banner of the hits due now, empty when none is due, bold red on a color terminal
func AdvisoryStartupBanner(ctx context.Context, hits []AdvisoryHit) string {
	due := AdvisoryHitsDueForBanner(ctx, hits, advisoryBannerRepeatHours)
	if len(due) == 0 {
		return ""
	}
	text := strings.Join(AdvisoryShortBannerLines(due), "\n")
	if advisoryTermSupportsColor() {
		return "\x1b[1;31m" + text + "\x1b[0m"
	}
	return text
}

// One line gateway log message for the hits not dismissed, empty when there is none
func AdvisoryGatewayLogMessage(ctx context.Context, hits []AdvisoryHit) string {
	fresh := FilterUnackedAdvisories(ctx, hits)
	if len(fresh) == 0 {
		return ""
	}
	if len(fresh) == 1 {
		h := fresh[0]
		return fmt.Sprintf("Security advisory [%s] active: %s==%s matches %s. See %s",
			h.Advisory.ID, h.Package, h.InstalledVersion, h.Advisory.Title, h.Advisory.URL)
	}
	ids := make([]string, 0, len(fresh))
	for _, h := range fresh {
		ids = append(ids, h.Advisory.ID)
	}
	return fmt.Sprintf("%d security advisories active (IDs: %s). Run `icebath doctor` on the gateway host for details.",
		len(fresh), strings.Join(ids, ", "))
}

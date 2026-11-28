package secrets

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One write into this process's environment for the profile in use, with what the variable held before it
type written struct {
	value, prior string
	had          bool
}

// What the stores supplied one profile, fetched the first time any variable of it is read
type scope struct {
	mu     sync.Mutex
	ready  bool
	report Report
	// Variables of the profile's .op.env
	fills map[string]string
	// Its status lines went where Serve said
	told bool
}

// What the stores supplied the profile and the variables of its .op.env, read now when they have not been; the status lines of the first
// fetch of the profile in use go where Serve said, the others to the log
func (h *Secrets) scoped(c *config.Config) (Report, map[string]string) {
	profile := c.Profile()
	h.mu.Lock()
	if !h.serving {
		// A process that serves no profiles, such as the app of a backend elsewhere, asks no store; nor does one before it serves
		h.mu.Unlock()
		return Report{}, gaps(c)
	}
	s := h.scopes[profile]
	if s == nil {
		s = &scope{}
		h.scopes[profile] = s
	}
	h.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		plain := c.Plain()
		s.fills = gaps(plain)
		s.report = h.apply(context.Background(), plain, h.unscoped(plain, s.fills), false)
		s.ready = true
		s.told = h.say(plain, s.report)
	}
	return s.report, s.fills
}

// Variables of this process as they were before the profile in use wrote its own
func (h *Secrets) base() map[string]string {
	env := config.Process()
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, w := range h.written {
		if current, set := env[name]; !set || current != w.value {
			continue
		}
		if w.had {
			env[name] = w.prior
		} else {
			delete(env, name)
		}
	}
	return env
}

// Variable of this process as the shell gave it, before the profile in use wrote its own
func (h *Secrets) Shell(name string) (string, bool) {
	current, set := os.LookupEnv(name)
	h.mu.Lock()
	w, ours := h.written[name]
	h.mu.Unlock()
	if !ours || !set || current != w.value {
		return current, set
	}
	return w.prior, w.had
}

// Variables that fill in what neither the shell nor the profile's .env sets: its .op.env unless a 1Password service account token is set
// already, then the .env beside the icebath program
func gaps(c *config.Config) map[string]string {
	_, fill := c.Layers()
	return fill
}

// Variables the profile has without any store: its .env over this process's own, which the .env beside the icebath program stands over when
// the profile has none, and the files that fill in what none of them sets
func (h *Secrets) unscoped(c *config.Config, gaps map[string]string) map[string]string {
	env := h.base()
	for name, value := range gaps {
		if _, set := env[name]; !set {
			env[name] = value
		}
	}
	over, _ := c.Layers()
	maps.Copy(env, over)
	maps.Copy(env, c.Dotenv())
	return env
}

// Value a store set for the profile and the store, when it is the one the profile reads: one a store set authoritatively wins over the
// .env, one set only because nothing held it yields to a value the .env gained since
func shown(report Report, dotenv map[string]string, name string) (string, string, bool) {
	applied, ok := report.Applied[name]
	if !ok {
		return "", "", false
	}
	if _, local := dotenv[name]; local && !applied.Authoritative {
		return "", "", false
	}
	return report.Values[name], applied.Source, true
}

// Value of the variable for the profile: what its stores set, else its .env, else the .env beside the icebath program when the profile has
// none, else this process's as it was before the profile in use wrote its own, else its .op.env and the .env beside the program
func (h *Secrets) Lookup(c *config.Config, name string) (string, bool) {
	report, gaps := h.scoped(c)
	dotenv := c.Dotenv()
	if value, _, ok := shown(report, dotenv, name); ok {
		return h.clean(name, value), true
	}
	if value, ok := dotenv[name]; ok {
		return h.clean(name, value), true
	}
	over, _ := c.Layers()
	if value, ok := over[name]; ok {
		return h.clean(name, value), true
	}
	if value, ok := h.Shell(name); ok {
		return h.clean(name, value), true
	}
	value, ok := gaps[name]
	return h.clean(name, value), ok
}

// Every variable of the profile
func (h *Secrets) Environ(c *config.Config) map[string]string {
	report, gaps := h.scoped(c)
	dotenv := c.Dotenv()
	env := h.unscoped(c, gaps)
	for name := range report.Applied {
		if value, _, ok := shown(report, dotenv, name); ok {
			env[name] = value
		}
	}
	for name, value := range env {
		env[name] = h.clean(name, value)
	}
	return env
}

// Store that supplied the variable the profile reads, by its short name, empty for one of the .env or this process
func (h *Secrets) Origin(c *config.Config, name string) string {
	report, _ := h.scoped(c)
	_, source, ok := shown(report, c.Dotenv(), name)
	if !ok {
		return ""
	}
	if found, known := h.Source(source); known {
		spec := found.Spec()
		return cmp.Or(spec.Short, spec.Label, source)
	}
	return source
}

// What the stores of the profile last did, fetched now when they have not been
func (h *Secrets) Report(c *config.Config) Report {
	report, _ := h.scoped(c)
	return report
}

// Makes this process's environment carry the profile in use from now on, its .env and what its stores supply written over what the
// shell gave; the status lines of this first fetch go to w
func (h *Secrets) Serve(w io.Writer) {
	h.mu.Lock()
	h.serving, h.out, h.active = true, w, h.config.Profile()
	active := h.active
	h.mu.Unlock()
	h.publish(active)
	h.mu.Lock()
	s := h.scopes[active]
	h.mu.Unlock()
	// Stores fetched before Serve, for a setting read as the profiles loaded, said their lines to the log only
	if s != nil {
		s.mu.Lock()
		if s.ready && !s.told {
			s.told = h.say(h.config.In(active).Plain(), s.report)
		}
		s.mu.Unlock()
	}
	h.mu.Lock()
	h.out = nil
	h.mu.Unlock()
}

// Makes this process's environment carry the profile now in use, taking back what the one before wrote
func (h *Secrets) Use(profile string) error {
	h.mu.Lock()
	h.active = profile
	serving := h.serving
	h.mu.Unlock()
	if serving {
		h.revoke()
		h.publish(profile)
	}
	return nil
}

// Forgets what the stores supplied the deleted profile
func (h *Secrets) Delete(profile string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.scopes, profile)
	return nil
}

// Forgets what the stores supplied a profile before its folder moves to a new name, its stores read again under that one
func (h *Secrets) Stop(profile string) error {
	return h.Delete(profile)
}

// Fetches the stores of the profile again, now for the profile in use, whose variables this process's environment carries again
func (h *Secrets) Reset(c *config.Config) Report {
	profile := c.Profile()
	h.mu.Lock()
	delete(h.scopes, profile)
	republish := h.serving && profile == h.active
	h.mu.Unlock()
	if republish {
		h.revoke()
		h.publish(profile)
	}
	return h.Report(c)
}

// Sets the variables for the profile as the store supplied them, over the .env and what was there, until its stores are fetched again:
// what a sync exports
func (h *Secrets) Export(c *config.Config, source string, values map[string]string) {
	report, gaps := h.scoped(c)
	pinned := Report{Sources: report.Sources, Conflicts: report.Conflicts, Values: maps.Clone(report.Values), Applied: maps.Clone(report.Applied)}
	for name, value := range values {
		pinned.Values[name] = value
		pinned.Applied[name] = Applied{Source: source, Authoritative: true}
	}
	profile := c.Profile()
	h.mu.Lock()
	h.scopes[profile] = &scope{ready: true, report: pinned, fills: gaps}
	republish := h.serving && profile == h.active
	h.mu.Unlock()
	if republish {
		h.revoke()
		h.publish(profile)
	}
}

// Writes the profile's variables into this process's environment, recording what each held before
func (h *Secrets) publish(profile string) {
	cfg := h.config.In(profile)
	report, gaps := h.scoped(cfg)
	dotenv := cfg.Dotenv()
	values := maps.Clone(dotenv)
	for name, value := range gaps {
		if _, local := dotenv[name]; !local {
			if _, set := h.Shell(name); !set {
				values[name] = value
			}
		}
	}
	for name := range report.Applied {
		if value, _, ok := shown(report, dotenv, name); ok {
			values[name] = value
		}
	}
	// What the administrator's .env sets wins over every other
	maps.Copy(values, cfg.ManagedEnv())
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(values)) {
		value := values[name]
		prior, had := os.LookupEnv(name)
		if had && prior == value {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			slog.Warn("A variable of the profile could not be set", "name", name, "error", err)
			continue
		}
		h.written[name] = written{value: value, prior: prior, had: had}
	}
}

// Takes back what the profile in use wrote into this process's environment, a variable changed since by anything else left as it is
func (h *Secrets) revoke() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, w := range h.written {
		if current, set := os.LookupEnv(name); set && current == w.value {
			if w.had {
				os.Setenv(name, w.prior)
			} else {
				os.Unsetenv(name)
			}
		}
	}
	clear(h.written)
}

// Credential with the characters outside ASCII taken out, which a header can't carry, said once for each variable
func (h *Secrets) clean(name, value string) string {
	if !config.Credential(name) || !strings.ContainsFunc(value, func(r rune) bool { return r > unicode.MaxASCII }) {
		return value
	}
	var kept strings.Builder
	var odd []string
	stripped := 0
	for _, r := range value {
		if r <= unicode.MaxASCII {
			kept.WriteRune(r)
			continue
		}
		stripped++
		label := fmt.Sprintf("U+%04X", r)
		if unicode.IsPrint(r) {
			label += " “" + string(r) + "”"
		}
		if len(odd) < 3 && !slices.Contains(odd, label) {
			odd = append(odd, label)
		}
	}
	h.mu.Lock()
	warned := h.warned[name]
	h.warned[name] = true
	h.mu.Unlock()
	if !warned {
		h.notice(fmt.Sprintf("  ⚠ %s held %s that a request can't carry: %s. icebath took them out of the key.", name,
			strs.Count(stripped, "character"), cmp.Or(strings.Join(odd, ", "), "characters that can't be printed")),
			"  This usually happens when a key is copied from a PDF, a word processor or a web page that swaps letters for ones that look\n"+
				"  the same. If the provider refuses the key, copy it again from the provider's website and set it again in /providers,\n"+
				"  or edit the keys file in a plain text editor.")
	}
	return kept.String()
}

// Lines told as icebath starts, to the log once it runs
func (h *Secrets) notice(lines ...string) {
	h.mu.Lock()
	out := h.out
	h.mu.Unlock()
	for _, line := range lines {
		if out != nil {
			fmt.Fprintln(out, line)
			continue
		}
		slog.Warn(strings.TrimSpace(line))
	}
}

// Says the status lines of a fetch: what each store set, why it failed and how to fix that, its warnings, and the variables two stores
// supplied; to where Serve said for the profile in use while it starts, true then, else to the log
func (h *Secrets) say(cfg *config.Config, report Report) bool {
	var lines []string
	for _, source := range report.Sources {
		if n := len(source.Applied); n > 0 {
			lines = append(lines, fmt.Sprintf("  ✓ %s set %s.", source.Label, strs.Count(n, "variable")))
		}
		if source.Result.Error != "" {
			lines = append(lines, fmt.Sprintf("  ⚠ %s: %s", source.Label, strs.Sentence(source.Result.Error)))
			if found, ok := h.Source(source.Name); ok {
				if hint := h.Remedy(cfg, found.Spec(), source.Result.Kind); hint != "" {
					lines = append(lines, "    "+strs.Sentence(hint))
				}
			}
		}
		for _, warning := range source.Result.Warnings {
			lines = append(lines, fmt.Sprintf("  ⚠ %s: %s", source.Label, strs.Sentence(warning)))
		}
	}
	for _, conflict := range report.Conflicts {
		lines = append(lines, "  ⚠ "+conflict)
	}

	h.mu.Lock()
	out := h.out
	active := h.active
	h.mu.Unlock()
	if out != nil && cfg.Profile() == active {
		for _, line := range lines {
			fmt.Fprintln(out, line)
		}
		return true
	}
	for _, line := range lines {
		slog.Info(strings.TrimSpace(line), "profile", cfg.Profile())
	}
	return false
}

package cron

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

const pinOutage = "Remote Config: cannot load the config for profile 'default' from https://plane: HTTP 503"

// Writes config.yaml into the store home and swaps the runtime provider resolver for a counting double
func pinStore(t *testing.T, mainModel, mainProvider string) (*Store, *int) {
	t.Helper()
	s, _ := newTestStore(t)
	writePinConfig(t, s, "model:\n  default: "+mainModel+"\n")
	calls := 0
	prev := pinRuntimeProvider
	pinRuntimeProvider = func(*Store) string { calls++; return mainProvider }
	t.Cleanup(func() { pinRuntimeProvider = prev })
	return s, &calls
}

func writePinConfig(t *testing.T, s *Store, text string) {
	t.Helper()
	if err := os.WriteFile(s.Home()+"/config.yaml", []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pinPair(j *Job) [2]string {
	return [2]string{deref(j.Model), deref(j.Provider)}
}

func TestPinnedTrueLocksThenPinnedFalseReleases(t *testing.T) {
	s, _ := pinStore(t, "main-model", "openrouter")
	unpinned, err := s.createJob(CreateJobOptions{Prompt: prompt("do a thing"), Schedule: "every 1 hour"})
	if err != nil {
		t.Fatal(err)
	}
	if unpinned.Model != nil || unpinned.Provider != nil {
		t.Fatalf("unpinned job carries a pin %v", pinPair(unpinned))
	}
	locked, err := s.updateJob(unpinned.ID, map[string]any{"pinned": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := pinPair(locked); got != [2]string{"main-model", "openrouter"} {
		t.Fatalf("pinned=True locked %v", got)
	}
	raw, err := os.ReadFile(s.JobsFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"pinned"`) {
		t.Fatalf("pinned is derived and never stored: %s", raw)
	}
	writePinConfig(t, s, "model:\n  default: newer-model\n")
	renamed, err := s.updateJob(locked.ID, map[string]any{"name": "renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if deref(renamed.Model) != "main-model" {
		t.Fatalf("locked job followed the main model to %q", deref(renamed.Model))
	}
	released, err := s.updateJob(locked.ID, map[string]any{"pinned": false})
	if err != nil {
		t.Fatal(err)
	}
	if released.Model != nil || released.Provider != nil {
		t.Fatalf("pinned=False kept %v", pinPair(released))
	}
}

func TestPinnedNeverOverridesAnExplicitModel(t *testing.T) {
	s, calls := pinStore(t, "main-model", "openrouter")
	job, err := s.createJob(CreateJobOptions{Prompt: prompt("do a thing"), Schedule: "every 1 hour", Model: "my-model", Provider: "nous", Pinned: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := pinPair(job); got != [2]string{"my-model", "nous"} {
		t.Fatalf("explicit model lost to the pin %v", got)
	}
	if *calls != 0 {
		t.Fatalf("resolver called %d times for an explicit model", *calls)
	}
	still, err := s.updateJob(job.ID, map[string]any{"pinned": true, "model": "other-model"})
	if err != nil {
		t.Fatal(err)
	}
	if deref(still.Model) != "other-model" {
		t.Fatalf("explicit update model lost %q", deref(still.Model))
	}
}

func TestCronPinRefusesInsteadOfDroppingTheLock(t *testing.T) {
	s, _ := newTestStore(t)
	prev := pinConfigExists
	pinConfigExists = func(context.Context, string) (bool, error) {
		return false, &config.ConfigBackendUnavailable{Message: pinOutage}
	}
	t.Cleanup(func() { pinConfigExists = prev })
	_, _, err := mainModelPin(s)
	if err == nil || !regexp.MustCompile(`cannot pin the job to the main model.*HTTP 503`).MatchString(err.Error()) {
		t.Fatalf("pin did not refuse: %v", err)
	}
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("refusal lost its cause %T", err)
	}
	_, err = s.createJob(CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local"), Pinned: true})
	if err == nil || !strings.Contains(err.Error(), "cannot pin") {
		t.Fatalf("create did not refuse: %v", err)
	}
	if jobs, err := s.listJobs(true); err != nil || len(jobs) != 0 {
		t.Fatalf("refused create stored %d jobs (%v)", len(jobs), err)
	}
}

func TestMainModelPinReadsEveryModelShape(t *testing.T) {
	cases := []struct {
		name, config, provider string
		want                   [2]*string
		resolved               int
	}{
		{"default key", "model:\n  default: m1\n", " OpenRouter ", [2]*string{prompt("openrouter"), prompt("m1")}, 1},
		{"model key fallback", "model:\n  default: ''\n  model: m2\n", "nous", [2]*string{prompt("nous"), prompt("m2")}, 1},
		{"bare string", "model: '  m3  '\n", "", [2]*string{nil, prompt("m3")}, 1},
		{"nothing configured", "agent:\n  max_turns: 3\n", "nous", [2]*string{nil, nil}, 0},
		{"non text model", "model:\n  default: 5\n", "nous", [2]*string{nil, nil}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := pinStore(t, "unused", tc.provider)
			writePinConfig(t, s, tc.config)
			prov, model, err := mainModelPin(s)
			if err != nil {
				t.Fatal(err)
			}
			if deref(prov) != deref(tc.want[0]) || (prov == nil) != (tc.want[0] == nil) {
				t.Fatalf("provider %v want %v", deref(prov), deref(tc.want[0]))
			}
			if deref(model) != deref(tc.want[1]) || (model == nil) != (tc.want[1] == nil) {
				t.Fatalf("model %v want %v", deref(model), deref(tc.want[1]))
			}
			if *calls != tc.resolved {
				t.Fatalf("resolver called %d times want %d", *calls, tc.resolved)
			}
		})
	}
}

func TestMainModelPinWithoutConfigStaysUnpinned(t *testing.T) {
	s, calls := pinStore(t, "unused", "nous")
	if err := os.Remove(s.Home() + "/config.yaml"); err != nil {
		t.Fatal(err)
	}
	prov, model, err := mainModelPin(s)
	if err != nil || prov != nil || model != nil || *calls != 0 {
		t.Fatalf("missing config pinned %v %v (%v, %d calls)", prov, model, err, *calls)
	}
}

func TestRuntimeProviderResolverSwallowsFailures(t *testing.T) {
	s, _ := newTestStore(t)
	s.loadConfig = func() (config.Tree, error) { return nil, errors.New("broken config") }
	if got := resolveMainRuntimeProvider(s); got != "" {
		t.Fatalf("broken config resolved %q", got)
	}
	s.loadConfig = func() (config.Tree, error) { panic("resolver blew up") }
	if got := resolveMainRuntimeProvider(s); got != "" {
		t.Fatalf("panicking resolution resolved %q", got)
	}
}

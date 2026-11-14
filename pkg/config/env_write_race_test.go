package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two party barrier whose timed out wait breaks it for every later waiter, as threading.Barrier does
type raceBarrier struct {
	mu      sync.Mutex
	arrived int
	broken  bool
	done    chan struct{}
}

func (b *raceBarrier) wait(timeout time.Duration) {
	b.mu.Lock()
	if b.broken {
		b.mu.Unlock()
		return
	}
	b.arrived++
	if b.arrived == 2 {
		close(b.done)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	select {
	case <-b.done:
	case <-time.After(timeout):
		b.mu.Lock()
		b.broken = true
		b.mu.Unlock()
	}
}

// Assignment names of a .env file, export prefixes dropped
func envKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\uFEFF"), "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "export ") {
			stripped = strings.TrimLeft(stripped[7:], " \t")
		}
		if name, _, ok := strings.Cut(stripped, "="); ok {
			keys[strings.TrimSpace(name)] = true
		}
	}
	return keys
}

// Temp home whose .env carries a BASE key, the race keys absent from the process env
func raceEnvHome(t *testing.T) (string, string) {
	t.Helper()
	h, _ := facadeHome(t, "")
	envPath := filepath.Join(h, ".env")
	if err := os.WriteFile(envPath, []byte("BASE_KEY=base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"RACE_A", "RACE_B", "DOOMED_KEY"} {
		t.Setenv(leaked, "")
		os.Unsetenv(leaked)
	}
	return h, envPath
}

// Runs two .env writers together, both pausing after their read until the other reads or the barrier breaks
func raceTwoWriters(t *testing.T, writerA, writerB func() error) {
	t.Helper()
	barrier := &raceBarrier{done: make(chan struct{})}
	original := readEnvLinesFile
	readEnvLinesFile = func(path string) ([]string, error) {
		lines, err := original(path)
		barrier.wait(time.Second)
		return lines, err
	}
	defer func() { readEnvLinesFile = original }()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, fn := range []func() error{writerA, writerB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- fn()
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("writer goroutines timed out")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writer goroutines raised: %v", err)
		}
	}
}

func TestConcurrentSaveEnvValueWritersDoNotLoseKeys(t *testing.T) {
	h, envPath := raceEnvHome(t)
	captureFacade(t)
	raceTwoWriters(t,
		func() error { return SaveEnvValue(h, "RACE_A", "a") },
		func() error { return SaveEnvValue(h, "RACE_B", "b") })
	if keys := envKeys(t, envPath); !reflect.DeepEqual(keys, map[string]bool{"BASE_KEY": true, "RACE_A": true, "RACE_B": true}) {
		t.Fatalf("lost a write to the unlocked read-modify-write race: %v", keys)
	}
}

func TestConcurrentRemoveAndSaveDoNotLoseKeys(t *testing.T) {
	h, envPath := raceEnvHome(t)
	captureFacade(t)
	if err := os.WriteFile(envPath, []byte("BASE_KEY=base\nDOOMED_KEY=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raceTwoWriters(t,
		func() error {
			_, err := RemoveHomeEnvValue(t.Context(), h, "DOOMED_KEY")
			return err
		},
		func() error { return SaveEnvValue(h, "RACE_B", "b") })
	if keys := envKeys(t, envPath); !reflect.DeepEqual(keys, map[string]bool{"BASE_KEY": true, "RACE_B": true}) {
		t.Fatalf("remove/save race produced a torn .env: %v", keys)
	}
}

// Non ASCII credential text is stripped with a warning and saved, never refused
func TestSaveEnvValueStripsNonASCIICredentialAndInvalidatesTheMemo(t *testing.T) {
	h, envPath := raceEnvHome(t)
	captureFacade(t)
	if got := LoadEnvFile(envPath); got["RACE_A"] != "" {
		t.Fatalf("memo seeded %v", got)
	}
	if err := SaveEnvValue(h, "RACE_A", "sk-\u0430bc"); err != nil {
		t.Fatalf("non ASCII value refused: %v", err)
	}
	if got := LoadEnvFile(envPath); got["RACE_A"] != "sk-bc" {
		t.Fatalf("saved %v", got)
	}
	if err := ValidateEnvNameForWrite("BAD'NAME"); err == nil || err.Error() != `Invalid environment variable name: "BAD'NAME"` {
		t.Fatalf("repr of the name %v", err)
	}
	if err := ValidateEnvNameForWrite("TRAILING\n"); err != nil {
		t.Fatalf("a dollar anchor accepts one trailing newline %v", err)
	}
}

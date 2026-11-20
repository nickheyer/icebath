package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

const secretsRecordingKind = "secretsrecording"

// File backend that logs every config read by home
type secretsRecordingBackend struct {
	config.FileBackend
}

var (
	secretsReadsMu   sync.Mutex
	secretsReads     []string
	secretsKindsOnce sync.Once
)

func (secretsRecordingBackend) record(h string) {
	secretsReadsMu.Lock()
	secretsReads = append(secretsReads, "read "+h)
	secretsReadsMu.Unlock()
}

func (b secretsRecordingBackend) ReadUserDoc(ctx context.Context, h string) (any, error) {
	b.record(h)
	return b.FileBackend.ReadUserDoc(ctx, h)
}

func (b secretsRecordingBackend) ReadUserDocReadonly(ctx context.Context, h string) (any, error) {
	b.record(h)
	return b.FileBackend.ReadUserDocReadonly(ctx, h)
}

// Selects the recording backend for the test and returns a reader of the logged reads
func recordSecretsReads(t *testing.T) func() []string {
	t.Helper()
	secretsKindsOnce.Do(func() {
		config.RegisterConfigBackend(secretsRecordingKind, func() config.ConfigBackend { return secretsRecordingBackend{} })
	})
	t.Setenv(config.BackendEnv, secretsRecordingKind)
	config.ResetCache()
	secretsReadsMu.Lock()
	secretsReads = nil
	secretsReadsMu.Unlock()
	t.Cleanup(config.ResetCache)
	return func() []string {
		secretsReadsMu.Lock()
		defer secretsReadsMu.Unlock()
		return slices.Clone(secretsReads)
	}
}

func TestDotenvSecretsReadGoesThroughTheSelectedBackend(t *testing.T) {
	launch := t.TempDir()
	routed := t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	for _, h := range []string{launch, routed} {
		body := "secrets:\n  vault:\n    enabled: true\n    home: '" + filepath.Base(h) + "'\n"
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reads := recordSecretsReads(t)
	for _, h := range []string{launch, routed} {
		cfg, err := loadSecretsConfig(h)
		if err != nil {
			t.Fatal(err)
		}
		vault, _ := cfg["vault"].(map[string]any)
		if vault["enabled"] != true || vault["home"] != filepath.Base(h) {
			t.Fatalf("secrets of %s = %v", h, cfg)
		}
		if !slices.Contains(reads(), "read "+h) {
			t.Fatalf("no backend read for %s in %v", h, reads())
		}
	}
	missing := t.TempDir()
	if cfg, err := loadSecretsConfig(missing); err != nil || len(cfg) != 0 || slices.Contains(reads(), "read "+missing) {
		t.Fatalf("missing config read %v %v %v", cfg, err, reads())
	}
}

func TestDotenvSecretsReadFailsClosedOnAnUnavailableBackend(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("secrets: {}\n"), 0o600)
	t.Setenv(config.BackendEnv, "bogus")
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	defer func() {
		var unavailable *config.ConfigBackendUnavailable
		err, _ := recover().(error)
		if !errors.As(err, &unavailable) {
			t.Fatalf("recovered %v", err)
		}
	}()
	loadSecretsConfig(h)
	t.Fatal("an unavailable backend served secrets")
}

func TestDotenvSecretsReadServesNothingFromABrokenFile(t *testing.T) {
	launch := t.TempDir()
	routed := t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	for _, h := range []string{launch, routed} {
		os.WriteFile(filepath.Join(h, "config.yaml"), []byte("secrets: [unterminated\n"), 0o600)
		if cfg, err := loadSecretsConfig(h); err != nil || len(cfg) != 0 {
			t.Fatalf("broken config of %s served %v %v", h, cfg, err)
		}
	}
}

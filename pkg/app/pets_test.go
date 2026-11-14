package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pet"
)

func petsTestRun(t *testing.T) *petsRun {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	os.Mkdir(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	return &petsRun{ctx: context.Background(), h: h, ns: map[string]any{}, w: &bytes.Buffer{}}
}

func TestPetsCliQuotedFalseDisablesAndToggleEnables(t *testing.T) {
	r := petsTestRun(t)
	os.WriteFile(filepath.Join(r.h, "config.yaml"), []byte("display:\n  pet:\n    enabled: 'false'\n    slug: ''\n    scale: 0.33\n"), 0o644)
	if active, err := r.hasActivePet(); err != nil || active {
		t.Fatalf("active %v %v", active, err)
	}
	enabled, name, msg, err := r.togglePetDisplay()
	if err != nil || msg == nil || !strings.Contains(*msg, "no pets installed") || enabled || name != nil {
		t.Fatalf("toggle %v %v %v %v", enabled, name, msg, err)
	}
}

func TestSetPetScaleWritesClampedValue(t *testing.T) {
	r := petsTestRun(t)
	applied, msg, err := r.setPetScale("0.5")
	if err != nil || msg != "" || applied != 0.5 {
		t.Fatalf("scale %v %q %v", applied, msg, err)
	}
	cfg, err := r.petConfig()
	if err != nil || cfg["scale"] != 0.5 {
		t.Fatalf("config %v %v", cfg, err)
	}
	if got, msg, _ := r.setPetScale(99); got != pet.MaxScale || msg != "" {
		t.Fatalf("max %v %q", got, msg)
	}
	if got, msg, _ := r.setPetScale(0); got != pet.MinScale || msg != "" {
		t.Fatalf("min %v %q", got, msg)
	}
}

//go:build !icebath_core && !icebath_noextras

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

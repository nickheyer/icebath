package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestFingerprintErrorEnumeratesParentCliSession(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	tmp := t.TempDir()
	source := filepath.Join(tmp, "busy.db")
	lfFreshStore(t, source)
	next := int64(0)
	swap(t, &recoverySourceFingerprint, func(string) *pyjson.Object {
		next++
		return pyjson.ObjectOf("main", pyjson.ObjectOf("size", next, "mtime_ns", next))
	})
	_, err := inspectSessionDatabase(context.Background(), source, tmp)
	var recErr *sessionRecoveryError
	if !errors.As(err, &recErr) || recErr.kind != "safety" {
		t.Fatalf("error %v", err)
	}
	message := recErr.Error()
	for _, want := range []string{"Stop every Icebath process", "CLI session", "fresh shell", "snapshot"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %q", want, message)
		}
	}
}

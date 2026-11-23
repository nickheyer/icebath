package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMemoryNotificationsNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, expected string
	}{
		{"None-on", "", "on"},
		{"on-on", "display:\n  memory_notifications: \"on\"\n", "on"},
		{"off-off", "display:\n  memory_notifications: \"off\"\n", "off"},
		{"verbose-verbose", "display:\n  memory_notifications: verbose\n", "verbose"},
		{"VERBOSE-verbose", "display:\n  memory_notifications: VERBOSE\n", "verbose"},
		{"True-on", "display:\n  memory_notifications: true\n", "on"},
		{"False-off", "display:\n  memory_notifications: false\n", "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			if tc.yaml != "" {
				if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := loadMemoryNotifications(context.Background()); got != tc.expected {
				t.Fatalf("got %q want %q", got, tc.expected)
			}
		})
	}
}

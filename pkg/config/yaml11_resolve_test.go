package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDocumentRefusesImpossibleTimestamps(t *testing.T) {
	cases := []struct{ body, err string }{
		{"when: 2024-02-30\n", "day is out of range for month"},
		{"when: 2023-02-29\n", "day is out of range for month"},
		{"when: 2024-13-01\n", "month must be in 1..12"},
		{"when: 0000-01-01\n", "year 0 is out of range"},
		{"2024-04-31: x\n", "day is out of range for month"},
		{"when: 2024-1-5 25:00:00\n", "hour must be in 0..23"},
		{"when: 2024-1-5T10:60:00\n", "minute must be in 0..59"},
		{"when: 2024-01-05 10:00:00 +24\n", "offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not datetime.timedelta(days=1)."},
		{"when: 2024-01-05 10:00:00 -24:30\n", "not datetime.timedelta(days=-2, seconds=84600)."},
		{"when: [2024-02-30]\n", "day is out of range for month"},
		{"when: 2024-02-29\n", ""},
		{"when: '2024-02-30'\n", ""},
		{"when: !!str 2024-02-30\n", ""},
		{"when: 2024-2-30\n", ""},
		{"when: 2024-01-05 10:00:00.5 -05:30\n", ""},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		doc, err := ReadDocument(path)
		if c.err == "" {
			if err != nil || doc == nil {
				t.Fatalf("%q: %v", c.body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Fatalf("%q: %v", c.body, err)
		}
	}
}

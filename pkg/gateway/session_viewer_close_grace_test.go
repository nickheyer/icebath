package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestSessionReapOptOutsHoldUntilTheOrphanEndGraceIsSet(t *testing.T) {
	for _, tc := range []struct {
		name            string
		yaml            string
		detach, startup float64
	}{
		{"unset", "", 300, 300},
		{"reap parked", "sessions:\n  ws_orphan_reap_grace_s: 0\n", 0, 0},
		{"reap negative", "sessions:\n  ws_orphan_reap_grace_s: -5\n", 0, 0},
		{"reap timed", "sessions:\n  ws_orphan_reap_grace_s: 45\n", 300, 300},
		{"sweep off", "sessions:\n  startup_orphan_sweep: false\n", 300, 0},
		{"sweep off text", "sessions:\n  startup_orphan_sweep: \"no\"\n", 300, 0},
		{"sweep on", "sessions:\n  startup_orphan_sweep: true\n", 300, 300},
		{"grace set", "sessions:\n  ws_orphan_reap_grace_s: 0\n  startup_orphan_sweep: false\n  orphan_end_grace_s: 60\n", 60, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db := newTestStateDB(t)
			if err := os.WriteFile(filepath.Join(filepath.Dir(db.Path()), "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			config.ResetCache()
			t.Cleanup(config.ResetCache)
			a := &SessionAuthority{DB: db}
			detach, err := orphanCloseGrace(ctx, a, false)
			if err != nil {
				t.Fatal(err)
			}
			startup, err := orphanCloseGrace(ctx, a, true)
			if err != nil {
				t.Fatal(err)
			}
			if detach != tc.detach || startup != tc.startup {
				t.Fatalf("detach %v startup %v, want %v %v", detach, startup, tc.detach, tc.startup)
			}
		})
	}
}

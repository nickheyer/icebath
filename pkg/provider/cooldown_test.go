package provider

import (
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestProviderResetDelay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if d, ok := ProviderResetDelay(1_700_000_090.5, now); !ok || d != 90500*time.Millisecond {
		t.Fatalf("future %v %v", d, ok)
	}
	if _, ok := ProviderResetDelay(1_699_999_999, now); ok {
		t.Fatal("past reset")
	}
	if _, ok := ProviderResetDelay(nil, now); ok {
		t.Fatal("nil reset")
	}
	if _, ok := ProviderResetDelay("junk", now); ok {
		t.Fatal("junk reset")
	}
	if MinSwitchResetFromConfig(config.Tree{"fallback": map[string]any{"min_switch_reset_seconds": "30"}}) != 30*time.Second {
		t.Fatal("string threshold")
	}
	if MinSwitchResetFromConfig(config.Tree{}) != 0 || MinSwitchResetFromConfig(nil) != 0 {
		t.Fatal("missing threshold is off")
	}
}

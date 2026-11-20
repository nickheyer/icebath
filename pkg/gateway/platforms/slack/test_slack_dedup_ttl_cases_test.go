package slack

import (
	"context"
	"os"
	"testing"
)

func TestDefaultTTLOutlastsSlackReconnectRedeliveryWindow(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("SLACK_DEDUP_TTL_SECONDS", "")
	os.Unsetenv("SLACK_DEDUP_TTL_SECONDS")
	if got := slackDedupTTLSeconds(context.Background()); got < 1800.0 {
		t.Fatalf("ttl %v", got)
	}
}

func TestEnvOverrideIsRespected(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("SLACK_DEDUP_TTL_SECONDS", "120")
	if got := slackDedupTTLSeconds(context.Background()); got != 120.0 {
		t.Fatalf("ttl %v", got)
	}
}

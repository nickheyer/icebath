//go:build !windows

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Route script answering which probed secrets reached it as the JSON payload that replaces the webhook body
const webhookEnvProbe = "#!/usr/bin/env bash\n" +
	`svc=other; [ "$SVC_TOKEN" = served-secret ] && svc=served; skill=other; [ "$SKILL_TOKEN" = skill-secret ] && skill=served` + "\n" +
	`printf '{"svc":"%s","skill":"%s","launch":"%s","key":"%s","home":"%s"}' "$svc" "$skill" "${LAUNCH_ONLY_TOKEN:-MISSING}" "${OPENROUTER_API_KEY:-}" "$ICEBATH_HOME"` + "\n"

func TestWebhookRouteScriptUnderMultiplexGetsTheServedProfilesSecretsWithoutFailingClosed(t *testing.T) {
	served := profileEnvServedHome(t, profileEnvLaunchHome(t))
	if err := os.MkdirAll(filepath.Join(served, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, filepath.Join(served, "scripts"), "probe.sh", webhookEnvProbe)
	ctx := ProfileRuntimeScope(context.Background(), served, true)
	accepted, transformed, err := NewWebhookRouteProcessor(WebhookDefaultScriptTimeoutSeconds).RunRouteScript(ctx, "probe.sh", pyjson.NewObject())
	if err != nil || !accepted || transformed == nil {
		t.Fatalf("route script accepted %v payload %v err %v", accepted, pyjson.Plain(transformed), err)
	}
	want := map[string]any{"svc": "served", "skill": "served", "launch": "launch-secret", "key": "", "home": served}
	if got := pyjson.Plain(transformed); !reflect.DeepEqual(got, want) {
		t.Fatalf("script saw %v, want %v", got, want)
	}
}

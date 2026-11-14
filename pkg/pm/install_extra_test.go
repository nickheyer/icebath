package pm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestColdRuntimeRefusalNamesTheExtra(t *testing.T) {
	tmp := ieClientEnv(t)
	t.Setenv(EnvDisableLazyInstalls, "1")
	_, err := request(context.Background(), "sync_venv", map[string]any{"extras": []any{"piper"}, "explicit": false, "repair": false}, workerHooks{}, tmp)
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(ie.Remedy, InstallHint("piper")) || !strings.Contains(ie.Cause, "piper") {
		t.Fatalf("refusal %#v", err)
	}
}

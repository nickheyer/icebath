package pm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestInstallExtraSyncsOnlyTheNamedExtras(t *testing.T) {
	_, calls := declSynced(t)
	code, err := CmdInstall(context.Background(), InstallArgs{Names: []string{}, Extra: []string{"otlp", "mcp", "otlp"}})
	if code != 0 || err != nil || !reflect.DeepEqual(*calls, [][]string{{"otlp", "mcp"}}) {
		t.Fatalf("install %d %v %v", code, err, *calls)
	}
}

func TestInstallExtraColdRuntimeRefusalNamesTheExtra(t *testing.T) {
	tmp := ieClientEnv(t)
	t.Setenv(EnvDisableLazyInstalls, "1")
	_, err := request(context.Background(), "sync_venv", map[string]any{"extras": []any{"bedrock"}, "explicit": false, "repair": false}, workerHooks{}, tmp)
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(ie.Remedy, InstallHint("bedrock")) || !strings.Contains(ie.Cause, "bedrock") {
		t.Fatalf("refusal %#v", err)
	}
}

package pm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExtraSyncsOnlyTheNamedExtras(t *testing.T) {
	_, calls := declSynced(t)
	code, err := CmdInstall(context.Background(), InstallArgs{Names: []string{}, Extra: []string{"silk", "documents", "silk"}})
	if code != 0 || err != nil || !reflect.DeepEqual(*calls, [][]string{{"silk", "documents"}}) {
		t.Fatalf("install %d %v %v", code, err, *calls)
	}
}

func TestColdRuntimeRefusalNamesTheExtra(t *testing.T) {
	tmp := ieClientEnv(t)
	t.Setenv(EnvDisableLazyInstalls, "1")
	_, err := request(context.Background(), "sync_venv", map[string]any{"extras": []any{"piper"}, "explicit": false, "repair": false}, workerHooks{}, tmp)
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(ie.Remedy, InstallHint("piper")) || !strings.Contains(ie.Cause, "piper") {
		t.Fatalf("refusal %#v", err)
	}
}

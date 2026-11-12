package pm

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestLegacyPostSwapInvocationRecognizesOnlyTheShippedShape(t *testing.T) {
	cases := []struct {
		argv []string
		path string
		tail []string
		ok   bool
	}{
		{[]string{"update", "--yes", "--branch", "main", "--post-swap", "/h/post_swap_1.json"}, "/h/post_swap_1.json", []string{"--yes", "--branch", "main"}, true},
		{[]string{"update", "--post-swap", "f.json"}, "f.json", []string{}, true},
		{[]string{"update", "--post-swap"}, "", nil, false},
		{[]string{"update", "--post-swap", "f.json", "--yes"}, "", nil, false},
		{[]string{"chat", "--post-swap", "f.json"}, "", nil, false},
		{[]string{"--post-swap", "f.json"}, "", nil, false},
		{nil, "", nil, false},
	}
	for _, c := range cases {
		path, tail, ok := LegacyPostSwapInvocation(c.argv)
		if ok != c.ok || path != c.path || (ok && !reflect.DeepEqual(tail, c.tail)) {
			t.Fatalf("%v -> %q %v %v", c.argv, path, tail, ok)
		}
	}
}

func TestTakeoverRequestMapsHistoricalPayloadNames(t *testing.T) {
	for _, desktop := range []bool{false, true} {
		for _, resume := range []any{nil, pyjson.ObjectOf("resume_needed", true)} {
			payload := pyjson.ObjectOf("gateway_mode", true, "had_desktop_app_before_update", desktop, "windows_gateway_resume", resume)
			request := takeoverRequest(payload, []string{"--yes"})
			for key, want := range map[string]any{"desktop": desktop, "assume_yes": true, "restart_update": false,
				"gateway_mode": true, "had_desktop_app_before_update": desktop, "windows_resume": resume} {
				if got, _ := request.Get(key); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %#v, want %#v", key, got, want)
				}
			}
		}
	}
	current := takeoverRequest(pyjson.ObjectOf("desktop", false, "windows_resume", "kept", "assume_yes", false, "restart_update", true), nil)
	for key, want := range map[string]any{"desktop": false, "windows_resume": "kept", "assume_yes": false, "restart_update": true} {
		if got, _ := current.Get(key); got != want {
			t.Fatalf("new schema %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestReadHandoffRefusesANonObjectPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "post_swap_9.json")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf[1]"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readHandoff(path)
	var py *PyError
	if !errors.As(err, &py) || py.Class != "ValueError" || py.Message != "post-swap hand-off "+path+" is not a JSON object" {
		t.Fatalf("err = %v", err)
	}
}

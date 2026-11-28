package server

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
)

func TestDeclaredSecretsMasked(t *testing.T) {
	value := []config.Setting{{Title: "Value", Default: "", Secret: true}}
	values := config.Setting{Key: "net.values", Default: map[string]any{}, Entry: &config.Entry{Fields: value}}
	servers := config.Setting{Key: "net.servers", Default: map[string]any{}, Entry: &config.Entry{Fields: []config.Setting{
		{Key: "command", Default: ""},
		{Key: "passphrase", Default: "", Secret: true},
		{Key: "values", Default: map[string]any{}, Entry: &config.Entry{Fields: value}},
	}}}
	long := "a-credential-of-some-length"
	hidden := mask(long)
	held := map[string]any{"one": map[string]any{"command": long, "passphrase": long, "token": long, "values": map[string]any{"PAT": long, "REF": "${PAT}"}}}
	want := map[string]any{"one": map[string]any{"command": long, "passphrase": hidden, "token": hidden, "values": map[string]any{"PAT": hidden, "REF": "${PAT}"}}}
	if got := shielded(servers, held, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("the servers read %v", got)
	}
	for path, part := range map[string]struct{ value, want any }{
		"one":            {held["one"], want["one"]},
		"one.command":    {long, long},
		"one.Passphrase": {long, hidden},
		"one.values":     {map[string]any{"PAT": long}, map[string]any{"PAT": hidden}},
		"one.values.PAT": {long, hidden},
		"one.other":      {long, long},
	} {
		if got := partShielded(servers, path, part.value, false); !reflect.DeepEqual(got, part.want) {
			t.Fatalf("%s reads %v", path, got)
		}
	}
	if got := shielded(values, map[string]any{"PAT": long}, false); !reflect.DeepEqual(got, map[string]any{"PAT": hidden}) {
		t.Fatalf("the values read %v", got)
	}
	if got := partShielded(values, "PAT", long, false); got != hidden {
		t.Fatalf("one value reads %v", got)
	}
}

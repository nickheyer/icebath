package gateway

import (
	"context"
	"reflect"
	"testing"
)

func entryNames(entries []*PlatformEntry) []string {
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestRegistryListingsKeepRegistrationOrder(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	r := NewPlatformRegistry()
	for _, n := range []string{"zulip", "alpha", "mid"} {
		r.register(NewPlatformEntry(n, n, nil, nil), "")
	}
	r.register(NewPlatformEntry("zulip", "again", nil, nil), "")
	scope := CurrentScopeKey(ctx)
	r.register(NewPlatformEntry("beta", "b", nil, nil), scope)
	r.register(NewPlatformEntry("alpha", "scoped", nil, nil), scope)
	got := r.AllEntries(ctx)
	if !reflect.DeepEqual(entryNames(got), []string{"zulip", "alpha", "mid", "beta"}) || got[0].Label != "again" || got[1].Label != "scoped" {
		t.Fatalf("all entries %v", entryNames(got))
	}
	r.unregister(context.Background(), "zulip", "")
	r.register(NewPlatformEntry("zulip", "z", nil, nil), "")
	if got := entryNames(r.LoadedEntries(context.Background())); !reflect.DeepEqual(got, []string{"alpha", "mid", "zulip"}) {
		t.Fatalf("re-registration after removal goes last %v", got)
	}
}

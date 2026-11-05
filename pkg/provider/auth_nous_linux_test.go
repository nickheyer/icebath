//go:build linux

package provider

import (
	"os"
	"testing"
)

func TestSharedStoreWriteAndReadRoundtrip(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	state := fullNousStateFixture()
	writeSharedNousState(env, state, false)
	path := nousSharedStorePath(env)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o", mode)
	}
	loaded, ok := readSharedNousState(env)
	if !ok || loaded["refresh_token"] != "refresh-tok" || loaded["access_token"] != state["access_token"] ||
		loaded["portal_base_url"] != "https://portal.example.com" || loaded["inference_base_url"] != "https://inference.example.com/v1" {
		t.Fatalf("loaded = %v", loaded)
	}
	if _, has := loaded["agent_key"]; has {
		t.Fatalf("agent_key persisted to the shared store")
	}
}

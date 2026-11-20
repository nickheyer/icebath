package provider

import (
	"testing"
)

func TestHomePoolEnvBindsStoreSecretsAndConfigOfTheHome(t *testing.T) {
	h := t.TempDir()
	env := HomePoolEnv(h)
	if env.Store == nil || env.Secrets == nil || env.Config == nil {
		t.Fatalf("pool env incomplete: %+v", env)
	}
	if env.Store.Home() != h {
		t.Fatalf("store home %q", env.Store.Home())
	}
	if env.Scope != nil || env.Log != nil {
		t.Fatal("home pool env carries no scope or logger")
	}
}

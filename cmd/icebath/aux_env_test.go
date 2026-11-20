package main

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// The Env the CLI and a gateway turn build reaches the auxiliary client, which resolves an explicit vision provider
func TestToolEnvsReachTheAuxiliaryClient(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	ctx := context.Background()
	for name, env := range map[string]*tools.Env{"cli": processToolsEnv(ctx), "gateway turn": {Home: h, Values: &sync.Map{}}} {
		aux, ok := env.AuxClient().(tools.AuxTaskResolver)
		if !ok {
			t.Fatalf("%s env auxiliary client %T", name, env.AuxClient())
		}
		if resolved, err := aux.ResolvesVision(tools.WithEnvHome(ctx, env), "openrouter"); err != nil || !resolved {
			t.Fatalf("%s env vision resolved %v, %v", name, resolved, err)
		}
	}
}

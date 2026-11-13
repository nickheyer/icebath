package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHandleReasoningCommandEffortDefaultsToSessionOnly(t *testing.T) {
	recordPrints(t)
	c := bareCLI()
	c.reasoningConfig = map[string]any{"enabled": true, "effort": "medium"}
	c.setAgent(&GatewayAgent{ctx: context.Background()})
	c.handleReasoningCommand("/reasoning high")

	if _, err := os.Stat(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml was written by a session scoped /reasoning, stat err %v", err)
	}
	if want := map[string]any{"enabled": true, "effort": "high"}; !reflect.DeepEqual(c.reasoningConfig, want) {
		t.Fatalf("reasoningConfig = %#v, want %#v", c.reasoningConfig, want)
	}
	if c.agentRef() != nil {
		t.Fatalf("agent = %#v, want nil", c.agentRef())
	}
}

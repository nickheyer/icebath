package observability_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestExecutionAdaptersDoNotCreateRelayHostWithoutAConsumer(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools.ResetRelayRuntimeForTests()
	imports := 0
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) {
		imports++
		return nil, errors.New("disabled execution adapter created Relay host")
	})
	t.Cleanup(func() {
		tools.ResetRelayRuntimeForTests()
		tools.RegisterNemoRelayBinding(nil)
	})
	ctx := context.Background()
	request := map[string]any{"model": "test-model", "messages": []any{}}
	response := &struct{ name string }{"response"}
	sessionID := "llm-session"
	got, err := provider.RelayExecute(ctx, request, func(_ context.Context, observed map[string]any) (any, error) {
		if reflect.ValueOf(observed).Pointer() == reflect.ValueOf(request).Pointer() {
			return response, nil
		}
		return nil, nil
	}, provider.RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: &sessionID})
	if err != nil || got != response {
		t.Fatalf("llm result %v %v", got, err)
	}
	toolArgs := map[string]any{"command": "true"}
	toolResult := &struct{ name string }{"tool"}
	result, observedArgs, err := tools.RelayExecuteTool(ctx, "terminal", toolArgs, func(_ context.Context, observed map[string]any) (any, error) {
		if reflect.ValueOf(observed).Pointer() == reflect.ValueOf(toolArgs).Pointer() {
			return toolResult, nil
		}
		return nil, nil
	}, "tool-session", "", nil)
	if err != nil || result != toolResult || reflect.ValueOf(observedArgs).Pointer() != reflect.ValueOf(toolArgs).Pointer() {
		t.Fatalf("tool result %v %v %v", result, observedArgs, err)
	}
	if host := tools.RelayHosts.ForProfile(ctx, "", false); host != nil {
		t.Fatalf("host %v", host)
	}
	if imports != 0 {
		t.Fatalf("imports %d", imports)
	}
}

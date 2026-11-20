package provider

import (
	"context"
	"errors"
	"testing"
)

// Out of tree client declaring both skip flags, unknown to every type switch of the auxiliary wrappers
type seamFlagClient struct{}

func (seamFlagClient) BaseURL() string { return "acp://seam-test" }
func (seamFlagClient) APIKey() string  { return "k" }
func (seamFlagClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("unused")
}
func (seamFlagClient) Close() error { return nil }
func (seamFlagClient) AuxDeclares(flag string) bool {
	return flag == "ICEBATH_SKIP_TRANSPORT_WRAP" || flag == "ICEBATH_SKIP_ASYNC_WRAP"
}

func TestSkipFlagsReplaceTheIsinstanceChecksForInAndOutOfTreeClients(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	gemini := &auxGeminiNativeClient{}
	if !auxClientDeclares(gemini, "ICEBATH_SKIP_TRANSPORT_WRAP") || auxClientDeclares(gemini, "ICEBATH_SKIP_ASYNC_WRAP") {
		t.Fatal("gemini native client flags")
	}
	client := seamFlagClient{}
	wrapped, err := auxMaybeWrapAnthropic(context.Background(), client, "m", "k", "acp://seam-test", "")
	if err != nil || wrapped != client {
		t.Fatalf("wrapped %v err %v", wrapped, err)
	}
	if async, _ := auxToAsyncClient(context.Background(), client, "m", false); async != client {
		t.Fatalf("async %v", async)
	}
}

package provider

import (
	"context"
	"reflect"
	"testing"
)

const (
	carrierRelay = "https://relay.example/v1"
	carrierOther = "https://other.example/v1"
)

// Registers the relay profile that declares the blob carriers and replays only the opaque one
func carrierRelayProfile(t *testing.T) {
	t.Helper()
	listProviders(context.Background())
	registerTestProfile(t, Profile{Name: "relay-fake", Hostnames: []string{"relay.example"}, MessageCarrierOpaque: "blob",
		MessageCarrierText: "blob_text", ReplayMessageCarriersHook: func(values map[string]any, _, _ string) map[string]any {
			if v, ok := values["blob"].(string); ok && v != "" {
				return map[string]any{"blob": v}
			}
			return map[string]any{}
		}})
}

func TestDeclaredCarriersAreCapturedAndReplayedOnlyOnTheClaimedEndpoint(t *testing.T) {
	carrierRelayProfile(t)
	ctx := context.Background()
	record := carrierRecord(ctx, map[string]any{"blob": "B", "blob_text": "T"})
	if !reflect.DeepEqual(record, map[string]any{"type": nativeCarrierType, "blob": "B", "blob_text": "T"}) {
		t.Fatalf("record %v", record)
	}
	assistant := map[string]any{"role": "assistant", "content": "x", "blob": "B", "reasoning_details": []any{record}}
	onRelay := shapeWireCarriers(ctx, []map[string]any{assistant}, "m", carrierRelay, nil)[0]
	elsewhere := shapeWireCarriers(ctx, []map[string]any{assistant}, "m", carrierOther, nil)[0]
	if onRelay["blob"] != "B" || onRelay["blob_text"] != nil || onRelay["reasoning_details"] != nil {
		t.Fatalf("relay wire %v", onRelay)
	}
	if _, ok := elsewhere["blob"]; ok || elsewhere["reasoning_details"] != nil {
		t.Fatalf("other wire %v", elsewhere)
	}
}

func TestPersistedCarrierKeysOfAnUninstalledPluginNeverReachTheWire(t *testing.T) {
	ctx := context.Background()
	record := map[string]any{"type": nativeCarrierType, "blob": "B"}
	assistant := map[string]any{"role": "assistant", "content": "x", "blob": "B", "reasoning_details": []any{record}}
	wire := shapeWireCarriers(ctx, []map[string]any{assistant}, "m", carrierRelay, nil)[0]
	if !reflect.DeepEqual(wire, map[string]any{"role": "assistant", "content": "x"}) {
		t.Fatalf("wire %v", wire)
	}
	if !reflect.DeepEqual(assistant["reasoning_details"], []any{record}) {
		t.Fatal("history was mutated")
	}
}

func TestShapeWireCarriersGeminiSignatureAndIdentity(t *testing.T) {
	ctx := context.Background()
	base := "https://generativelanguage.googleapis.com/v1beta/openai"
	record := map[string]any{"type": nativeCarrierType, "extra_content": map[string]any{"google": map[string]any{"thought_signature": "SIG"}}}
	row := map[string]any{"role": "assistant", "content": "391", "reasoning_details": []any{record}}
	wire := shapeWireCarriers(ctx, []map[string]any{row}, "gemini-3.5-pro", base, nil)[0]
	if !reflect.DeepEqual(wire["extra_content"], record["extra_content"]) || wire["reasoning_details"] != nil {
		t.Fatalf("gemini wire %v", wire)
	}
	legacy := map[string]any{"role": "assistant", "content": "a", "reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "t"},
		map[string]any{"type": "hermes.native_assistant", "extra_content": map[string]any{"thought_signature": "OLD"}}}}
	stripped := shapeWireCarriers(ctx, []map[string]any{legacy}, "gpt-5.4", carrierOther, nil)[0]
	if _, ok := stripped["extra_content"]; ok || !reflect.DeepEqual(stripped["reasoning_details"], []any{map[string]any{"type": "reasoning.text", "text": "t"}}) {
		t.Fatalf("non gemini wire %v", stripped)
	}
	signed := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": "{}", "thought_signature": "S"}}}}
	out := shapeWireCarriers(ctx, []map[string]any{signed}, "gpt-5.4", carrierOther, nil)[0]
	fn := out["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, ok := fn["thought_signature"]; ok || fn["name"] != "f" {
		t.Fatalf("function signature survived %v", fn)
	}
	plain := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "yo"}}
	if got := shapeWireCarriers(ctx, plain, "m", carrierOther, nil); reflect.ValueOf(got).Pointer() != reflect.ValueOf(plain).Pointer() {
		t.Fatal("an unchanged history must come back as the input itself")
	}
}

func TestStreamCarriersKeepLatestOpaqueAndJoinText(t *testing.T) {
	carrierRelayProfile(t)
	s := newStreamCarriers(context.Background())
	var reasoning *string
	reasoning = s.feed(map[string]any{"blob": "first", "blob_text": "Think "}, reasoning)
	reasoning = s.feed(map[string]any{"model_extra": map[string]any{"blob": "second", "blob_text": "more"},
		"extra_content": map[string]any{"google": map[string]any{"thought_signature": "SIG"}}}, reasoning)
	if reasoning == nil || *reasoning != "Think " {
		t.Fatalf("reasoning %v", reasoning)
	}
	message := map[string]any{"role": "assistant"}
	s.apply(map[string]any{"choices": []any{map[string]any{"message": message}}})
	want := map[string]any{"role": "assistant", "blob": "second", "blob_text": "Think more",
		"extra_content": map[string]any{"google": map[string]any{"thought_signature": "SIG"}}}
	if !reflect.DeepEqual(message, want) {
		t.Fatalf("message %v", message)
	}
	if carrierThoughtSignature(map[string]any{"thought_signature": "  "}) != "" || carrierThoughtSignature(map[string]any{"thought_signature": "F"}) != "F" {
		t.Fatal("flat signature handling")
	}
}

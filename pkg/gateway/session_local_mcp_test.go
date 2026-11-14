package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Field and detail of a RuntimeStoreError, failing the test on any other error
func storeRefusal(t *testing.T, err error) (string, string, string) {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		t.Fatalf("want RuntimeStoreError, got %v", err)
	}
	field, detail := "", ""
	if refusal.Field != nil {
		field = *refusal.Field
	}
	if refusal.Detail != nil {
		detail = *refusal.Detail
	}
	return refusal.Reason, field, detail
}

func TestValidateEditor(t *testing.T) {
	if err := validateEditor("cli", nil); err != nil {
		t.Fatal(err)
	}
	stdio := map[string]any{"name": "fs", "command": "/bin/fs", "args": []any{"-v"}, "env": []any{}}
	if err := validateEditor("acp", map[string]any{"mcp_servers": []any{stdio}, "edit_approval_policy": "ask"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		source        string
		editor        any
		field, detail string
	}{
		{"cli", map[string]any{}, "editor", "an editor policy is only accepted from an ACP launch, not cli"},
		{"acp", []any{}, "editor", "must be a mapping, got list"},
		{"acp", map[string]any{"zeta": 1, "alpha": 2}, "editor.alpha", "unknown editor key"},
		{"acp", map[string]any{"edit_approval_policy": "auto"}, "editor.edit_approval_policy", "must be 'ask': every edit is approved one at a time"},
		{"acp", map[string]any{"mcp_servers": "x"}, "editor.mcp_servers", "must be a list, got str"},
		{"acp", map[string]any{"mcp_servers": []any{stdio, stdio}}, "editor.mcp_servers", "unusable server spec: duplicate server"},
	} {
		reason, field, detail := storeRefusal(t, validateEditor(tc.source, tc.editor))
		if reason != "invalid_params" || field != tc.field || detail != tc.detail {
			t.Fatalf("%v: %s %s %s", tc.editor, reason, field, detail)
		}
	}
}

func TestValidateServers(t *testing.T) {
	many := make([]any, 17)
	for _, tc := range []struct {
		servers any
		detail  string
	}{
		{map[string]any{}, "must be a list, got dict"},
		{many, "at most 16 servers, got 17"},
		{[]any{map[string]any{"name": strings.Repeat("x", 70000)}}, "the serialized server list exceeds 65536 bytes"},
		{[]any{"spec"}, "unusable server spec: AttributeError"},
		{[]any{map[string]any{"type": "ws"}}, "unusable server spec: KeyError"},
		{[]any{map[string]any{"type": nil}}, "unusable server spec: KeyError"},
		{[]any{map[string]any{"type": []any{}}}, "unusable server spec: TypeError"},
		{[]any{map[string]any{}}, "unusable server spec: args: missing; command: missing; env: missing; name: missing"},
		{[]any{map[string]any{"name": "a", "command": 3, "args": []any{"ok", 4}, "env": []any{"E", map[string]any{"name": "N"}}, "_meta": 1}},
			"unusable server spec: _meta: dict_type; args.1: string_type; command: string_type; env.0: model_type; env.1.value: missing"},
		{[]any{map[string]any{"type": "http", "name": "h", "headers": "x"}}, "unusable server spec: headers: list_type; url: missing"},
		{[]any{map[string]any{"type": "sse", "name": "", "url": "u", "headers": []any{}}}, "unusable server spec: duplicate server"},
	} {
		_, field, detail := storeRefusal(t, validateServers(tc.servers))
		if field != "editor.mcp_servers" || detail != tc.detail[:min(len(tc.detail), 160)] {
			t.Fatalf("%v: %s %q", tc.servers, field, detail)
		}
	}
	ok := []any{
		map[string]any{"name": "a", "command": "c", "args": []any{}, "env": []any{map[string]any{"name": "K", "value": "V"}}, "type": "stdio"},
		map[string]any{"type": "http", "name": "b", "url": "https://x", "headers": []any{}, "extra": true},
	}
	if err := validateServers(ok); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateEditorRequest(t *testing.T) {
	servers := []any{map[string]any{"name": "a", "command": "c", "args": []any{"--token", "s3cret"}, "env": []any{}}}
	request := map[string]any{"editor": map[string]any{"mcp_servers": servers, "edit_approval_policy": "ask"}}
	if reason, _, _ := storeRefusal(t, privateEditorRequest(request, nil)); reason != "launch_credentials_unavailable" {
		t.Fatal(reason)
	}
	private := &RecoveredSecrets{}
	if err := privateEditorRequest(request, private); err != nil {
		t.Fatal(err)
	}
	digest, _ := editorMCPFingerprint(servers)
	editor := request["editor"].(map[string]any)
	if fp := editor["mcp_servers"].(map[string]any)["fingerprint"]; fp != digest || editor["edit_approval_policy"] != "ask" {
		t.Fatalf("editor %v", editor)
	}
	held, ok := private.Get(editorMCPPrivateKey)
	if !ok || held.([]any)[0].(map[string]any)["args"].([]any)[1] != "s3cret" {
		t.Fatalf("held %v", held)
	}
	if len(digest) != 64 {
		t.Fatal(digest)
	}
	empty := map[string]any{"editor": nil}
	if err := privateEditorRequest(empty, nil); err != nil || empty["editor"] != nil {
		t.Fatal(err)
	}
}

func TestPrivateMutationPayload(t *testing.T) {
	servers := []any{map[string]any{"type": "sse", "name": "s", "url": "u", "headers": []any{}}}
	stored, held, err := privateMutationPayload("editor_mcp", map[string]any{"mcp_servers": servers})
	if err != nil || held == nil {
		t.Fatal(err)
	}
	digest, _ := editorMCPFingerprint(servers)
	if stored.(map[string]any)["mcp_servers"].(map[string]any)["fingerprint"] != digest {
		t.Fatalf("stored %v", stored)
	}
	if reason, _, _ := storeRefusal(t, func() error {
		_, _, err := privateMutationPayload("editor_mcp", map[string]any{"mcp_servers": servers, "x": 1})
		return err
	}()); reason != "invalid_params" {
		t.Fatal(reason)
	}
	payload := map[string]any{"title": "t"}
	if stored, held, err := privateMutationPayload("branch", payload); err != nil || held != nil || stored.(map[string]any)["title"] != "t" {
		t.Fatalf("%v %v %v", stored, held, err)
	}
	if stored, held, err := privateMutationPayload("rename", map[string]any{"mcp_servers": "x"}); err != nil || held != nil || stored == nil {
		t.Fatalf("%v %v %v", stored, held, err)
	}
	if _, _, err := privateMutationPayload("branch", map[string]any{"mcp_servers": "x"}); err == nil {
		t.Fatal("a branch with an unusable list passed")
	}
}

package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
)

// Seconds an admission start waits on a held server's tools list
const editorRelistTimeout = 5 * time.Second

// Whether any borrowed editor server list is held by the authority
func borrowsEditorMCP(a *SessionAuthority) bool {
	a.editorMCP.mu.Lock()
	defer a.editorMCP.mu.Unlock()
	return len(a.editorMCP.borrowed) > 0
}

// Rechecks the editor schemas before the head is claimed, a drifted manifest refrozen and a failure only logged
func refreezeEditorMCP(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) {
	if !borrowsEditorMCP(a) {
		return
	}
	if err := refreezeEditor(ctx, a, ref); err != nil {
		slog.Warn(fmt.Sprintf("Editor MCP re-check of session %s failed; the turn runs on its frozen manifest: %s", ref.SessionID, pyExceptionRepr(err)))
	}
}

// Probes the borrowed servers of the committed policy and refreezes a drifted manifest
func refreezeEditor(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	receipt, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	raw, err := pyItem(receipt, "policy")
	if err != nil {
		return err
	}
	policy, err := restorePolicy(raw)
	if err != nil {
		return err
	}
	data, err := policyEditorManifest(policy)
	if err != nil || len(data) == 0 {
		return err
	}
	scope := pyrepr.Str(data["scope"])
	servers, ok := borrowedEditorMCP(a, scope)
	if !ok {
		return nil
	}
	specs := editorSpecs(servers)
	names, err := frozenEditorNames(data, specs)
	if err != nil {
		return err
	}
	var schemas map[string]any
	complete, drifted := false, false
	err = borrowEditorMCP(ctx, a, scope, specs, names, policy.Cwd, true, func(s map[string]any, connected []int) error {
		schemas, complete = s, len(connected) == len(names)
		if complete {
			listed := make([]string, 0, len(names))
			for _, name := range names {
				listed = append(listed, name)
			}
			drifted = mcp.EditorToolsDrifted(scope, listed, editorRelistTimeout)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !complete || (pyjson.Equal(schemas, pyjson.Plain(data["schemas"])) && !drifted) {
		return nil
	}
	return commitRefreeze(ctx, a, ref, servers)
}

// Commits the editor_mcp change on the owning principal's behalf through the mutation writer
func commitRefreeze(ctx context.Context, a *SessionAuthority, ref SessionRef, servers any) error {
	owner, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	principal, err := pyItem(owner, "principal_id")
	if err != nil {
		return err
	}
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	payload, copied, err := privateMutationPayload("editor_mcp", map[string]any{"mcp_servers": servers})
	if err != nil {
		return err
	}
	params, err := fencedMutation(row, ref.SessionID, "editor-mcp-refreeze:"+uuidHex(), "editor_mcp", payload)
	if err != nil {
		return err
	}
	_, err = writeMutation(ctx, a, ref, params, copied, pyrepr.Str(principal), nil, nil)
	return err
}

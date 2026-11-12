//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// One pre authority session row with its source and user id, empty for none
type seededSession struct{ id, source, userID string }

// Writes rows the way the pre authority clients did, with no route and no owner binding
func seedSessions(t *testing.T, h string, rows []seededSession) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range rows {
		if err := db.CreateSession(state.NewSession{ID: row.id, Source: row.source, UserID: row.userID}); err != nil {
			t.Fatal(err)
		}
		for _, message := range []map[string]any{{"role": "user", "content": "OLD_" + row.id}, {"role": "assistant", "content": "REPLY_" + row.id}} {
			if _, err := db.AppendMessage(ctx, row.id, message, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Random hex identifier, the uuid4 hex of the hermes case
func uuidHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func TestPerProfileGrantsEditOldLocalRowsOfTheirOwnProfile(t *testing.T) {
	model := map[string]any{"model": map[string]any{"provider": "custom", "default": "m", "base_url": "http://127.0.0.1:1/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}}}
	rootConfig := map[string]any{"gateway": map[string]any{"multiplex_profiles": true}}
	for k, v := range model {
		rootConfig[k] = v
	}
	tmp, h, user := gatewayCaseHomes(t, rootConfig)
	beta := filepath.Join(h, "profiles", "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONConfig(t, filepath.Join(beta, "config.yaml"), model)
	seedSessions(t, h, []seededSession{{"old-desktop-root", "desktop", ""}, {"old-tui-root", "tui", ""}})
	seedSessions(t, beta, []seededSession{{"old-desktop-beta", "desktop", ""}, {"old-oneshot-beta", "oneshot", ""},
		{"old-desktop-nous", "desktop", "nous:account"}, {"old-desktop-foreign", "desktop", "telegram:42"}})
	daemon := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), gatewayChildEnv(h, user), gateway.GatewayRunModuleEntry)
	descriptor := daemon.waitReady(t, h, 60*time.Second)
	homes := map[string]string{}
	for name, dir := range map[string]string{"default": h, "beta": beta} {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		homes[name] = resolved
	}
	ticket := func(profile, purpose string) string {
		grant, err := gatewayControl(h, "session-ticket", map[string]any{"profile_id": homes[profile], "purpose": purpose,
			"instance_id": descriptor["instance_id"]})
		if err != nil {
			t.Fatal(err)
		}
		value, _ := grant["ticket"].(string)
		return value
	}
	origin, _ := descriptor["api_origin"].(string)
	client := newOriginClient(t, origin)
	pin := func(sid, profile, mintedFor string) int {
		status, body := client.call("GET", fmt.Sprintf("/api/sessions/%s/mutation-snapshot?profile=%s", sid, profile),
			http.Header{nativeTicketHeader: {ticket(mintedFor, "native-http")}}, "")
		if status != 200 {
			return status
		}
		var snapshot map[string]any
		if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
			t.Fatal(err)
		}
		edit, _ := json.Marshal(map[string]any{"profile": profile, "pinned": true, "request_id": uuidHex(t),
			"expected_revision": snapshot["runtime_revision"]})
		status, _ = client.call("PATCH", "/api/sessions/"+sid, http.Header{nativeTicketHeader: {ticket(mintedFor, "native-http")},
			"Content-Type": {"application/json"}}, string(edit))
		return status
	}
	for _, row := range [][2]string{{"old-desktop-root", "default"}, {"old-tui-root", "default"}, {"old-desktop-beta", "beta"},
		{"old-oneshot-beta", "beta"}, {"old-desktop-nous", "beta"}} {
		wantStatus(t, "pin "+row[0]+" in "+row[1], pin(row[0], row[1], row[1]), 200)
	}
	wantStatus(t, "pin a beta row with a default grant", pin("old-desktop-beta", "beta", "default"), 403)
	wantStatus(t, "pin a foreign identity row", pin("old-desktop-foreign", "beta", "beta"), 403)
	ws, _ := dialSession(t, h, wsOrigin(origin), map[string]any{"profile_id": homes["default"], "purpose": "interactive",
		"instance_id": descriptor["instance_id"]})
	listed := sessionResult(t, ws, "session.list", map[string]any{"scope": "stored", "limit": 50})
	got := map[string]bool{}
	rows, _ := listed["sessions"].([]any)
	for _, raw := range rows {
		if row, isDict := raw.(map[string]any); isDict {
			sid, _ := row["session_id"].(string)
			got[sid] = true
		}
	}
	if want := map[string]bool{"old-desktop-root": true, "old-tui-root": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored picker = %v", listed)
	}
}

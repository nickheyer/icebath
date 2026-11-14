package gateway

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const forwardedSubject = `auth:v1:["basic","","wweic"]`

// Control server for home holding a ticket store that serves home itself
func forwardedServer(t *testing.T, h string) *GatewayControlServer {
	t.Helper()
	server, err := NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if server.TicketStore, err = NewTicketStore("inst-1", map[string]bool{h: true}); err != nil {
		t.Fatal(err)
	}
	return server
}

// Session ticket request line naming the profile and, when withSubject is set, a forwarded subject
func mintLine(t *testing.T, profile string, subject any, withSubject bool) []byte {
	t.Helper()
	params := map[string]any{"profile_id": profile, "instance_id": "inst-1", "purpose": "interactive"}
	if withSubject {
		params["subject"] = subject
	}
	line, err := json.Marshal(map[string]any{"protocol": 1, "id": 7, "verb": "session-ticket", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// Decoded answer of one request line
func forwardedAnswer(t *testing.T, server *GatewayControlServer, line []byte, peer string) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(server.HandleRequestLine(line, peer), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestAVerifiedPeerMintsForTheNamedDashboardPrincipal(t *testing.T) {
	h := t.TempDir()
	server := forwardedServer(t, h)
	response := forwardedAnswer(t, server, mintLine(t, h, forwardedSubject, true), "uid:501")
	if response["ok"] != true {
		t.Fatalf("response %v", response)
	}
	grant, err := server.TicketStore.Redeem(response["result"].(map[string]any)["ticket"], nil, "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if grant["subject"] != forwardedSubject || !grant["capabilities"].(map[string]bool)["session:create"] {
		t.Fatalf("grant %v", grant)
	}
}

func TestAnUnverifiedPeerCannotNameAPrincipal(t *testing.T) {
	h := t.TempDir()
	response := forwardedAnswer(t, forwardedServer(t, h), mintLine(t, h, forwardedSubject, true), "")
	if response["ok"] != false || !strings.Contains(response["error"].(string), "authenticated runtime bootstrap unavailable") {
		t.Fatalf("response %v", response)
	}
}

func TestAMalformedForwardedSubjectFailsClosed(t *testing.T) {
	h := t.TempDir()
	for _, bad := range []any{"", strings.Repeat("x", 257), 501, nil} {
		response := forwardedAnswer(t, forwardedServer(t, h), mintLine(t, h, bad, true), "uid:501")
		if response["ok"] != false || !strings.Contains(response["error"].(string), "invalid bootstrap subject") {
			t.Fatalf("%v: response %v", bad, response)
		}
	}
}

func TestABareForwardedLabelFailsClosed(t *testing.T) {
	h := t.TempDir()
	for _, bad := range []string{"admin", "basic:wweic", "uid:501", `auth:v1:"basic"`, "auth:v1:[1,2,3]"} {
		response := forwardedAnswer(t, forwardedServer(t, h), mintLine(t, h, bad, true), "uid:501")
		if response["ok"] != false || !strings.Contains(response["error"].(string), "invalid bootstrap subject") {
			t.Fatalf("%v: response %v", bad, response)
		}
	}
}

func TestAForwardedSubjectMatchingTheRowOwnerTakesTheMutation(t *testing.T) {
	h := t.TempDir()
	sameOwner, parse, agree := state.SameOwner, state.ParseAuthenticatedSubject, state.OwnersAgree
	state.SameOwner, state.ParseAuthenticatedSubject, state.OwnersAgree = SameOwner, ParseAuthenticatedSubject, OwnersAgree
	t.Cleanup(func() { state.SameOwner, state.ParseAuthenticatedSubject, state.OwnersAgree = sameOwner, parse, agree })
	db, err := state.NewSessionDB(t.Context(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "cold-desktop", Source: "desktop", UserID: forwardedSubject}); err != nil {
		t.Fatal(err)
	}
	capabilities := []string{"session:read", "session:create"}
	same := state.HistoryActor{ProfileID: h, Subject: forwardedSubject, Capabilities: capabilities}
	other := state.HistoryActor{ProfileID: h, Subject: `auth:v1:["basic","","someone-else"]`, Capabilities: capabilities}
	if err := db.ReadCtx(t.Context(), func(conn state.SQLiteConn) error {
		ok, err := state.AuthorizeHistory(t.Context(), conn, same, "cold-desktop", false)
		if err != nil || !ok {
			t.Fatalf("same owner %v %v", ok, err)
		}
		_, err = state.AuthorizeHistory(t.Context(), conn, other, "cold-desktop", false)
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) || refusal.Reason != "permission_denied" {
			t.Fatalf("a foreign principal took the mutation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

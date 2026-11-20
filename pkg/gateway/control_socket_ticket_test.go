package gateway

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const loginSubject = `auth:v1:["basic","","wweic"]`

// Control server for home holding a ticket store that serves home itself
func ticketServer(t *testing.T, h string) *GatewayControlServer {
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

// Session ticket request line naming the profile plus any extra params
func mintLine(t *testing.T, profile string, extra map[string]any) []byte {
	t.Helper()
	params := map[string]any{"profile_id": profile, "instance_id": "inst-1", "purpose": "interactive"}
	for key, value := range extra {
		params[key] = value
	}
	line, err := json.Marshal(map[string]any{"protocol": 1, "id": 7, "verb": "session-ticket", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// Decoded answer of one request line
func ticketAnswer(t *testing.T, server *GatewayControlServer, line []byte, peer string) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(server.HandleRequestLine(line, peer), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestAVerifiedPeerMintsForItsOwnSubject(t *testing.T) {
	h := t.TempDir()
	server := ticketServer(t, h)
	response := ticketAnswer(t, server, mintLine(t, h, nil), "uid:501")
	if response["ok"] != true {
		t.Fatalf("response %v", response)
	}
	grant, err := server.TicketStore.Redeem(response["result"].(map[string]any)["ticket"], nil, "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if grant["subject"] != "uid:501" || !grant["capabilities"].(map[string]bool)["session:create"] {
		t.Fatalf("grant %v", grant)
	}
}

func TestAnUnverifiedPeerCannotMint(t *testing.T) {
	h := t.TempDir()
	response := ticketAnswer(t, ticketServer(t, h), mintLine(t, h, nil), "")
	if response["ok"] != false || !strings.Contains(response["error"].(string), "authenticated runtime bootstrap unavailable") {
		t.Fatalf("response %v", response)
	}
}

func TestANamedSubjectIsRefused(t *testing.T) {
	h := t.TempDir()
	response := ticketAnswer(t, ticketServer(t, h), mintLine(t, h, map[string]any{"subject": loginSubject}), "uid:501")
	if response["ok"] != false || !strings.Contains(response["error"].(string), "invalid bootstrap parameters") {
		t.Fatalf("response %v", response)
	}
}

func TestALoginStampedRowAnswersOnlyItsOwnSubject(t *testing.T) {
	h := t.TempDir()
	sameOwner, parse, agree := state.SameOwner, state.ParseAuthenticatedSubject, state.OwnersAgree
	state.SameOwner, state.ParseAuthenticatedSubject, state.OwnersAgree = SameOwner, ParseAuthenticatedSubject, OwnersAgree
	t.Cleanup(func() { state.SameOwner, state.ParseAuthenticatedSubject, state.OwnersAgree = sameOwner, parse, agree })
	db, err := state.NewSessionDB(t.Context(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "cold-history", Source: state.ClientHistorySource, UserID: loginSubject}); err != nil {
		t.Fatal(err)
	}
	capabilities := []string{"session:read", "session:create"}
	same := state.HistoryActor{ProfileID: h, Subject: loginSubject, Capabilities: capabilities}
	other := state.HistoryActor{ProfileID: h, Subject: `auth:v1:["basic","","someone-else"]`, Capabilities: capabilities}
	if err := db.ReadCtx(t.Context(), func(conn state.SQLiteConn) error {
		ok, err := state.AuthorizeHistory(t.Context(), conn, same, "cold-history", false)
		if err != nil || !ok {
			t.Fatalf("same owner %v %v", ok, err)
		}
		_, err = state.AuthorizeHistory(t.Context(), conn, other, "cold-history", false)
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) || refusal.Reason != "permission_denied" {
			t.Fatalf("a foreign principal took the mutation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Connections and account routes recording every mint
type accountsGateway struct {
	connected map[string]bool
	mints     []map[string]any
}

func (g *accountsGateway) ListConnectors(context.Context, time.Duration) ([]map[string]any, error) {
	return []map[string]any{{"connector": "gmail", "enabled": true, "connected": g.connected["gmail"]}}, nil
}

func (g *accountsGateway) Connections(_ context.Context, req ConnectionsRequest) (map[string]any, error) {
	mint := map[string]any{"connectors": strings.Join(req.Connectors, ","), "reinitiate": req.Reinitiate, "alias": req.Alias}
	if req.ConnectionID != "" {
		mint["connection_id"] = req.ConnectionID
	}
	g.mints = append(g.mints, mint)
	var results []any
	for _, c := range req.Connectors {
		results = append(results, map[string]any{"connector": c, "status": "initiated",
			"connection_id": fmt.Sprintf("ca_%s_%d", c, len(g.mints)), "connect_url": "https://connect.example/" + c})
	}
	return map[string]any{"results": results}, nil
}

func (g *accountsGateway) AccountStatus(_ context.Context, id string, _ time.Duration) (map[string]any, error) {
	return map[string]any{"connectionId": id, "connector": "gmail", "status": "active", "label": "a@example.com", "active": true,
		"createdAt": "2026-10-08T00:00:00Z", "updatedAt": "2026-10-08T00:00:00Z"}, nil
}

func portalAccount(alias any, label, status string) map[string]any {
	key := label
	if s, ok := alias.(string); ok && s != "" {
		key = s
	}
	return map[string]any{"connectionId": "ca_" + key, "connector": "gmail", "status": status, "label": label, "alias": alias,
		"active": status == "active", "createdAt": "t", "updatedAt": "t"}
}

func runAccounts(t *testing.T, env *Env, args map[string]any, gw *accountsGateway, accounts []map[string]any, accountsErr error) map[string]any {
	old := PortalAccounts
	PortalAccounts = func(context.Context, *Env) ([]map[string]any, error) { return accounts, accountsErr }
	t.Cleanup(func() { PortalAccounts = old })
	return runManaged(t, context.Background(), env, args, gw, func(*pyjson.Object) string { return "" }, 0, "desktop")
}

func aliasTarget(alias string, extra ...string) map[string]any {
	t := map[string]any{"name": "gmail", "alias": alias}
	if len(extra) > 0 {
		t["to"] = extra[0]
	}
	return t
}

func mintWant(reinitiate bool, alias string, connectionID string) map[string]any {
	m := map[string]any{"connectors": "gmail", "reinitiate": reinitiate, "alias": alias}
	if connectionID != "" {
		m["connection_id"] = connectionID
	}
	return m
}

func TestAccountsConnectWithAnAliasAsksForThatAccountAndWatchesTheOneItMints(t *testing.T) {
	env := managedTestSetup(t)
	gw := &accountsGateway{connected: map[string]bool{}}
	out := runAccounts(t, env, connectArgs("connect", aliasTarget("work")), gw, nil, nil)
	row := targetsOf(out)["gmail"]
	if !reflect.DeepEqual(gw.mints, []map[string]any{mintWant(false, "work", "")}) || row["alias"] != "work" || row["state"] != "connected" {
		t.Fatalf("mints %v out %v", gw.mints, out)
	}
}

func TestAccountsReconnectWithAnAliasRepairsItEvenWhenAnotherAccountIsHealthy(t *testing.T) {
	env := managedTestSetup(t)
	gw := &accountsGateway{connected: map[string]bool{"gmail": true}}
	accounts := []map[string]any{portalAccount("home", "me@example.com", "active"), portalAccount("work", "me@corp.example", "expired")}
	runAccounts(t, env, connectArgs("reconnect", aliasTarget("work")), gw, accounts, nil)
	if !reflect.DeepEqual(gw.mints, []map[string]any{mintWant(true, "", "ca_work")}) {
		t.Fatalf("mints %v", gw.mints)
	}
}

func TestAccountsReconnectOfAnUnnamedAccountByItsLabelRepairsThatAccountByID(t *testing.T) {
	env := managedTestSetup(t)
	gw := &accountsGateway{connected: map[string]bool{"gmail": true}}
	accounts := []map[string]any{portalAccount("home", "me@example.com", "active"), portalAccount(nil, "gmail_old-label", "expired")}
	runAccounts(t, env, connectArgs("reconnect", aliasTarget("gmail_old-label")), gw, accounts, nil)
	if !reflect.DeepEqual(gw.mints, []map[string]any{mintWant(true, "", "ca_gmail_old-label")}) {
		t.Fatalf("mints %v", gw.mints)
	}
}

func TestAccountsReconnectOfAHealthyUnnamedAccountByLabelMintsNothingUnlessForced(t *testing.T) {
	env := managedTestSetup(t)
	gw := &accountsGateway{connected: map[string]bool{}}
	accounts := []map[string]any{portalAccount("home", "me@example.com", "active"), portalAccount(nil, "gmail_old-label", "active")}
	runAccounts(t, env, connectArgs("reconnect", aliasTarget("gmail_old-label")), gw, accounts, nil)
	if len(gw.mints) != 0 {
		t.Fatalf("mints %v", gw.mints)
	}
	args := connectArgs("reconnect", aliasTarget("gmail_old-label"))
	args["force"] = true
	runAccounts(t, env, args, gw, accounts, nil)
	if !reflect.DeepEqual(gw.mints, []map[string]any{mintWant(true, "", "ca_gmail_old-label")}) {
		t.Fatalf("forced mints %v", gw.mints)
	}
}

func TestAccountsAnOpenRepairOfOneAccountIsNotReusedForAnother(t *testing.T) {
	managedTestSetup(t)
	target := NewConnTarget("gmail", "connector", "reconnect")
	repairID := "ca_a"
	target.RepairID = &repairID
	repair := NewConnectionOperation([]*ConnTarget{target}, "s1", "", nil)
	if err := LiveOpen(context.Background(), repair); err != nil {
		t.Fatal(err)
	}
	a, b := "ca_a", "ca_b"
	if LiveFindTarget("gmail", nil, &a, "") != repair || LiveFindTarget("gmail", nil, &b, "") != nil {
		t.Fatal("repair reused for another account")
	}
}

func TestAccountsReconnectOfANameNoAccountHasYetSendsTheAlias(t *testing.T) {
	env := managedTestSetup(t)
	gw := &accountsGateway{connected: map[string]bool{}}
	runAccounts(t, env, connectArgs("reconnect", aliasTarget("work")), gw, []map[string]any{portalAccount("home", "me@example.com", "active")}, nil)
	if !reflect.DeepEqual(gw.mints, []map[string]any{mintWant(true, "work", "")}) {
		t.Fatalf("mints %v", gw.mints)
	}
}

func TestAccountsReconnectWithAnAliasMintsNothingWhenTheAccountListCannotBeRead(t *testing.T) {
	env := managedTestSetup(t)
	gw := &accountsGateway{connected: map[string]bool{}}
	out := runAccounts(t, env, connectArgs("reconnect", aliasTarget("work")), gw, nil, &ToolGatewayError{Message: "down", Status: 503})
	if _, has := out["error"]; len(gw.mints) != 0 || !has {
		t.Fatalf("mints %v out %v", gw.mints, out)
	}
}

func TestAccountsRenameSendsTheNewNameForTheResolvedAccount(t *testing.T) {
	env := managedTestSetup(t)
	type sentReq struct {
		method, url string
		body        any
	}
	var sent []sentReq
	transport := func(_ context.Context, method, rawURL string, headers map[string]string, body any, _ time.Duration) (*http.Response, error) {
		raw, _ := json.Marshal(body)
		var decoded any
		_ = json.Unmarshal(raw, &decoded)
		sent = append(sent, sentReq{method, rawURL, decoded})
		reply, _ := json.Marshal(map[string]any{"connectionId": "ca_work", "connector": "gmail", "status": "active", "label": "me@corp.example",
			"alias": body.(map[string]any)["alias"], "active": true, "createdAt": "t", "updatedAt": "t"})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(reply))}, nil
	}
	oldFactory := PortalAccountClientFactory
	PortalAccountClientFactory = func(ctx context.Context, env *Env) PortalAccountClient {
		return NewPortalConnectorClient(ctx, env, PortalOptions{Transport: transport,
			EndpointResolver: func() (string, error) { return "https://portal.test", nil },
			HeaderProvider:   func(string) map[string]string { return map[string]string{"Authorization": "Bearer t"} }})
	}
	defer func() { PortalAccountClientFactory = oldFactory }()
	accounts := []map[string]any{portalAccount("home", "me@example.com", "active"), portalAccount("work", "me@corp.example", "active")}
	out := runAccounts(t, env, connectArgs("rename", aliasTarget("work", "office")), &accountsGateway{}, accounts, nil)
	want := []sentReq{{"PATCH", "https://portal.test/api/v1/connectors/accounts/ca_work", map[string]any{"alias": "office"}}}
	if !reflect.DeepEqual(sent, want) || !reflect.DeepEqual(out["renamed"], map[string]any{"connector": "gmail", "alias": "office", "label": "me@corp.example"}) {
		t.Fatalf("sent %v out %v", sent, out)
	}
}

type refusingRename struct{ t *testing.T }

func (r refusingRename) ListAccounts() ([]map[string]any, error) { return nil, nil }
func (r refusingRename) RenameAccount(string, string) (map[string]any, error) {
	r.t.Fatal("rename called")
	return nil, nil
}

func TestAccountsRenameOfAnUnknownNameListsTheNamesThatExist(t *testing.T) {
	env := managedTestSetup(t)
	oldFactory := PortalAccountClientFactory
	PortalAccountClientFactory = func(context.Context, *Env) PortalAccountClient { return refusingRename{t} }
	defer func() { PortalAccountClientFactory = oldFactory }()
	accounts := []map[string]any{portalAccount("home", "me@example.com", "active"), portalAccount(nil, "me@corp.example", "active")}
	out := runAccounts(t, env, connectArgs("rename", aliasTarget("wrk", "office")), &accountsGateway{}, accounts, nil)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "home") || !strings.Contains(msg, "me@corp.example") {
		t.Fatalf("out %v", out)
	}
}

func TestAccountsAnOpenOperationForOneAccountIsNotReusedForAnother(t *testing.T) {
	managedTestSetup(t)
	plain := NewConnectionOperation([]*ConnTarget{NewConnTarget("gmail", "connector", "connect")}, "s1", "", nil)
	if err := LiveOpen(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	work := "work"
	if LiveFindTarget("gmail", nil, nil, "") != plain || LiveFindTarget("gmail", &work, nil, "") != nil {
		t.Fatal("plain operation reused for a named account")
	}
}

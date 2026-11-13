package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type modelReply struct {
	status int
	header http.Header
	body   string
}

type modelSent struct {
	url     string
	header  http.Header
	body    map[string]any
	rawBody string
}

// Scripted round tripper answering each request with the next reply and recording what was sent
type modelTransport struct {
	mu      sync.Mutex
	replies []modelReply
	sent    []modelSent
}

func (m *modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.Do(req)
}

func (m *modelTransport) Do(req *http.Request) (*http.Response, error) {
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, modelSent{req.URL.String(), req.Header.Clone(), body, string(raw)})
	r := m.replies[0]
	m.replies = m.replies[1:]
	header := r.header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: r.status, Header: header, Body: io.NopCloser(bytes.NewReader([]byte(r.body))), Request: req}, nil
}

func TestConnectorWireModelsDriveTheGatewayClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	for _, k := range []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_SOURCE", "ICEBATH_SESSION_ID"} {
		t.Setenv(k, "")
	}
	env := &Env{Home: home, Values: &sync.Map{}}
	ctx := context.Background()

	t.Setenv("CONNECTOR_GATEWAY_URL", "https://gw.example")
	if u, err := NewConnectorClientFrom(env, ConnectorClientOptions{}).url(connectorsPath); err != nil || u != "https://gw.example/v1/connectors" {
		t.Fatalf("default resolver url %q %v", u, err)
	}
	t.Setenv("CONNECTOR_GATEWAY_URL", "")
	t.Setenv("TOOL_GATEWAY_SCHEME", "ftp")
	if _, err := NewConnectorClient(env, nil, "").url(connectorsPath); err == nil || err.(*ToolGatewayError).Code != "NO_ORIGIN" {
		t.Fatalf("invalid scheme should resolve no origin, got %v", err)
	}

	gateway := &modelTransport{}
	storage := &modelTransport{}
	var slept []time.Duration
	client := NewConnectorClientFrom(env, ConnectorClientOptions{
		Transport:        &http.Client{Transport: gateway},
		Storage:          storage,
		EndpointResolver: func() string { return "https://gw.test" },
		HeaderProvider: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"Authorization": "Bearer t"}, nil
		},
		Sleep: func(d time.Duration) { slept = append(slept, d) },
	})

	gateway.replies = []modelReply{{status: 200, body: `{"results":[{"index":0,"use_case":"find mail","tools":["GMAIL_SEND"],"extra":1}],` +
		`"schemas":{"GMAIL_SEND":{"connector":"gmail","tool":"GMAIL_SEND","input_schema":{"type":"object"}}},"nextSteps":["connect"]}`}}
	found, err := client.Search(ctx, []map[string]any{{"use_case": "find mail"}})
	if err != nil {
		t.Fatal(err)
	}
	if gateway.sent[0].rawBody != `{"queries": [{"useCase": "find mail"}]}` {
		t.Fatalf("search body %s", gateway.sent[0].rawBody)
	}
	first := found["results"].([]any)[0].(map[string]any)
	if first["use_case"] != "find mail" || first["index"] != 0.0 || first["guidance"] != nil || len(found["connections"].([]any)) != 0 ||
		!reflect.DeepEqual(found["next_steps"], []any{"connect"}) {
		t.Fatalf("search dump %#v", found)
	}
	if schema := found["schemas"].(map[string]any)["GMAIL_SEND"].(map[string]any); schema["description"] != "" || schema["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("schema dump %#v", schema)
	}

	gateway.replies = []modelReply{{status: 200, body: `{"results":[{"useCase":"x"}]}`}}
	_, err = client.Search(ctx, []map[string]any{{"use_case": "x"}})
	var ge *ToolGatewayError
	if !errors.As(err, &ge) || ge.Message != "invalid connector search: results.0.index: Field required" || ge.Code != "INVALID_RESPONSE" || ge.Status != 200 {
		t.Fatalf("malformed search error %#v", err)
	}

	before := len(gateway.sent)
	_, err = client.Connections(ctx, ConnectionsRequest{Connectors: []string{"gmail"}, ReturnTo: "cli"})
	var verr *pydantic.ValidationError
	if !errors.As(err, &verr) || len(gateway.sent) != before {
		t.Fatalf("an unknown return target must fail validation before sending, got %v", err)
	}

	gateway.replies = []modelReply{
		{status: 429, header: http.Header{"Retry-After": {"2"}}, body: `{"code":"PROVIDER_RATE_LIMITED","message":"slow"}`},
		{status: 200, body: `{"result":{"connector":"gmail","tool":"SEND","account":"work","dataUse":{"requires":["no_training"]},` +
			`"data":{"file":{"name":"a/b.txt","mimetype":"text/plain","url":"https://storage.test/x","expiresAt":"z"},"n":1}}}`},
	}
	before = len(gateway.sent)
	results, err := client.Execute(ctx, []PlannedCall{{Position: 0, Name: "connectors__gmail__SEND", Connector: "gmail", Tool: "SEND", Arguments: map[string]any{"to": "a"}}}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(slept, []time.Duration{2 * time.Second}) {
		t.Fatalf("retry wait %v", slept)
	}
	retry, again := gateway.sent[before], gateway.sent[before+1]
	if retry.rawBody != `{"call": {"connector": "gmail", "tool": "SEND", "arguments": {"to": "a"}}}` || retry.header.Get("x-idempotency-key") == "" ||
		retry.header.Get("x-idempotency-key") != again.header.Get("x-idempotency-key") {
		t.Fatalf("execute attempts %#v %#v", retry, again)
	}
	result := results[0]
	if result["account"] != "work" || !reflect.DeepEqual(result["data_use"], []string{"no_training"}) || result["error"] != nil {
		t.Fatalf("execute result %#v", result)
	}

	storage.replies = []modelReply{{status: 200, body: "hello"}}
	saved := SaveOutputs(ctx, env, client, result)
	if len(storage.sent) != 1 || storage.sent[0].url != "https://storage.test/x" || storage.sent[0].header.Get("Authorization") != "" {
		t.Fatalf("storage request %#v", storage.sent)
	}
	data := saved["data"].(*pyjson.Object)
	file := data.Vals["file"].(*pyjson.Object)
	if !reflect.DeepEqual(data.Keys, []string{"file", "n"}) || !reflect.DeepEqual(file.Keys, []string{"name", "mimetype", "path"}) {
		t.Fatalf("saved keys %v %v", data.Keys, file.Keys)
	}
	path := file.Vals["path"].(string)
	if filepath.Dir(path) != filepath.Join(home, "downloads", "connectors") || !strings.HasSuffix(path, "-b.txt") {
		t.Fatalf("saved path %s", path)
	}
	if body, _ := os.ReadFile(path); string(body) != "hello" {
		t.Fatalf("saved body %q", body)
	}
	if next, err := reserveDownloadName(filepath.Dir(path), "b.txt"); err != nil || !strings.HasSuffix(next, "-b-1.txt") && !strings.HasSuffix(next, "-b.txt") {
		t.Fatalf("reserved name %s %v", next, err)
	}

	gateway.replies = []modelReply{{status: 200, body: `{"results":[{"index":0,"data":{"ok":true}},` +
		`{"index":1,"error":{"code":"CONNECTION_REQUIRED","message":"connect","connectUrl":"https://c"}}]}`}}
	legacy, err := client.executeLegacy(ctx, []PlannedCall{{Connector: "gmail", Tool: "A"}, {Connector: "gmail", Tool: "B"}}, "hermes-desktop", "op1")
	if err != nil {
		t.Fatal(err)
	}
	if body := gateway.sent[len(gateway.sent)-1].body; body["returnTo"] != "hermes-desktop" || body["op"] != "op1" || len(body["tools"].([]any)) != 2 {
		t.Fatalf("legacy body %#v", body)
	}
	if e := legacy[1]["error"].(map[string]any); e["code"] != "CONNECTION_REQUIRED" || e["connect_url"] != "https://c" || e["family"] != "product" {
		t.Fatalf("legacy error %#v", legacy[1])
	}

	if p := PartitionCalls("connectors__gmail__SEND"); len(p.Remote)+len(p.Local)+len(p.Errors) != 0 {
		t.Fatalf("a string batch must not split into characters %#v", p)
	}
	p := PartitionCalls([]any{map[string]any{"name": "connectors__gmail__SEND", "arguments": map[string]any{"to": "a"}}})
	if len(p.Remote) != 1 {
		t.Fatalf("partition %#v", p)
	}
	if entry := RenderRemoteEntry(ctx, p.Remote[0], map[string]any{"data": 1, "account": "work"}); entry["account"] != "work" || entry["response"] != 1 {
		t.Fatalf("rendered entry %#v", entry)
	}

	if e, err := ParseGatewayError(429, map[string]any{"retryAfterMs": 0.0, "code": "RATE"}, nil); err != nil || e.Kind != gatewayKindRateLimited || e.RetryAfter != 1.0 || e.Code != "RATE" {
		t.Fatalf("a zero retryAfterMs waits the default second %#v", e)
	}
	if e, err := ParseGatewayError(429, nil, http.Header{"Retry-After": {" 3 "}}); err != nil || e.RetryAfter != 3 || e.Code != "HTTP_429" {
		t.Fatalf("retry after header %#v", e)
	}
	if e, err := ParseGatewayError(429, nil, http.Header{"Retry-After": {"Wed, 21 Oct 2015 07:28:00 GMT"}}); e != nil || err == nil ||
		err.Error() != "could not convert string to float: 'Wed, 21 Oct 2015 07:28:00 GMT'" {
		t.Fatalf("a date in Retry-After is the float conversion failure, got %#v %v", e, err)
	}
	if e, err := ParseGatewayError(503, nil, http.Header{"Retry-After": {"soon"}}); err != nil || !e.Retryable {
		t.Fatalf("only a 429 reads Retry-After %#v %v", e, err)
	}
	if e, err := ParseGatewayError(503, nil, nil); err != nil || !e.Retryable || e.Kind != "" || e.Status != 503 {
		t.Fatalf("server failure %#v", e)
	}
	if e, err := ParseGatewayError(401, map[string]any{"error": "unauthorized"}, nil); err != nil || e.Kind != gatewayKindAuth || e.Code != "unauthorized" || e.Retryable {
		t.Fatalf("auth failure %#v", e)
	}
	if e := newToolGatewayError("", "boom", "", 0, "", false); e.Code != "GATEWAY_ERROR" {
		t.Fatalf("default code %#v", e)
	}

	for raw, want := range map[any]bool{false: false, "garbage": true, nil: true} {
		if got := connectorConfigFromRaw(raw).Enabled; got != want {
			t.Fatalf("config of %#v enabled %v", raw, got)
		}
	}
	if connectorConfigFromRaw(map[string]any{"enabled": "off"}).Enabled {
		t.Fatal("enabled off must disable connectors")
	}

	ModelParameters("connectors__gmail__SEND", map[string]any{"type": "object", "properties": map[string]any{"attachment": map[string]any{"file_uploadable": true}}})
	_, err = StageInputs(ctx, env, client, PlannedCall{Name: "connectors__gmail__SEND", Connector: "gmail", Tool: "SEND",
		Arguments: map[string]any{"attachment": map[string]any{"name": "x", "mimetype": "m", "s3key": "k"}}}, "")
	var fe *FileInputError
	if !errors.As(err, &fe) || fe.Code != "FILE_NOT_ALLOWED" || fe.Hint != "Pass the path of the local file to send." {
		t.Fatalf("descriptor input %#v", err)
	}
	var staged []string
	_, err = substituteFileInput(map[string]any{"file": stagedDescriptor{"name": "n.txt", "s3key": "k"}}, []string{"file", "name"},
		func(raw string) (any, error) { staged = append(staged, raw); return raw, nil })
	if err != nil || !reflect.DeepEqual(staged, []string{"n.txt"}) {
		t.Fatalf("a path through a staged descriptor must reach its leaf %v %v", staged, err)
	}
}

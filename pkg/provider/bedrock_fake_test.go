package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	bedrockFakeAccessKey = "AKIAFAKEE2EBEDROCK01"
	bedrockFakeSecretKey = "fake/e2e/secret/never+real/0000000000000"
	bedrockFakeRegion    = "us-east-1"
)

var (
	bedrockFakeRouteRe = regexp.MustCompile(`^/model/([^/]+)/(converse|converse-stream|invoke|invoke-with-response-stream|count-tokens)$`)
	bedrockFakeOps     = map[string]string{"converse": "Converse", "converse-stream": "ConverseStream", "invoke": "InvokeModel",
		"invoke-with-response-stream": "InvokeModelWithResponseStream", "count-tokens": "CountTokens"}
	bedrockFakeAuthRe = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([a-z0-9-]+)/([a-z0-9-]+)/aws4_request, ?SignedHeaders=([a-z0-9;-]+), ?Signature=([0-9a-f]{64})$`)
)

// Botocore bedrock-runtime shapes, operations and exception statuses loaded from testdata
type bedrockServiceModel struct {
	Errors     map[string]int               `json:"errors"`
	Operations map[string]map[string]string `json:"operations"`
	Shapes     map[string]map[string]any    `json:"shapes"`
}

var (
	bedrockModel *bedrockServiceModel
)

// Rejection carries the AWS error code and message the fake answers with
type bedrockRejection struct{ Code, Message string }

func (e *bedrockRejection) Error() string { return e.Code + ": " + e.Message }

// Scripted content blocks of one assistant turn
type bedrockBlock interface{ bedrockBlock() }

type bedrockText struct {
	Text  string
	Chunk int
}

type bedrockReasoning struct{ Text, Signature string }

type bedrockToolUse struct {
	Name      string
	Input     map[string]any
	ToolUseID string
}

func (*bedrockText) bedrockBlock()      {}
func (*bedrockReasoning) bedrockBlock() {}
func (*bedrockToolUse) bedrockBlock()   {}

// Replies the responder scripts for each call
type bedrockReply interface{ bedrockReply() }

type bedrockTurn struct {
	Blocks          []bedrockBlock
	StopReason      string
	InputTokens     int
	OutputTokens    int
	GuardrailAction string
}

type bedrockHTTPError struct{ Code, Message string }

type bedrockStreamException struct {
	Turn               *bedrockTurn
	Exception, Message string
	After              int
}

type bedrockDrop struct {
	Turn  *bedrockTurn
	After int
	Clean bool
}

func (*bedrockTurn) bedrockReply()            {}
func (*bedrockHTTPError) bedrockReply()       {}
func (*bedrockStreamException) bedrockReply() {}
func (*bedrockDrop) bedrockReply()            {}

// One recorded call with its verified auth scope, rejection and emitted blocks
type bedrockFakeRecord struct {
	Path     string
	Headers  map[string]string
	Raw      []byte
	T        time.Time
	Op       string
	Model    string
	Body     map[string]any
	Auth     map[string]string
	Rejected string
	Reply    string
	Emitted  []map[string]any
}

type bedrockResponder func(rec *bedrockFakeRecord) bedrockReply

// Recording validating Bedrock Runtime fake answering Converse and InvokeModel calls
type bedrockFake struct {
	t             testing.TB
	responder     bedrockResponder
	bearer        string
	mu            sync.Mutex
	requests      []*bedrockFakeRecord
	signatures    map[string]string
	reasonedTools map[string]bool
	ids           int
	srv           *httptest.Server
}

func (f *bedrockFake) endpoint() string { return f.srv.URL }

// Endpoint override and the fake static credentials for a client under test
func (f *bedrockFake) clientEnv() map[string]string {
	return map[string]string{"AWS_ENDPOINT_URL_BEDROCK_RUNTIME": f.endpoint(), "AWS_ACCESS_KEY_ID": bedrockFakeAccessKey,
		"AWS_SECRET_ACCESS_KEY": bedrockFakeSecretKey, "AWS_REGION": bedrockFakeRegion}
}

// Applies the client environment to the test
func (f *bedrockFake) setEnv(t *testing.T) {
	for k, v := range f.clientEnv() {
		t.Setenv(k, v)
	}
}

func (f *bedrockFake) snapshot() []*bedrockFakeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*bedrockFakeRecord(nil), f.requests...)
}

func (f *bedrockFake) nextID(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids++
	return fmt.Sprintf("%s%04d%05d", prefix, f.ids, time.Now().UnixMilli()%100000)
}

func (f *bedrockFake) record(rec *bedrockFakeRecord) {
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()
}

// Resolves ids and signatures and returns the Converse content blocks of the turn
func (f *bedrockFake) materialize(turn *bedrockTurn) []map[string]any {
	var out []map[string]any
	reasoned, tools := false, false
	for _, b := range turn.Blocks {
		switch b := b.(type) {
		case *bedrockText:
			out = append(out, map[string]any{"text": b.Text})
		case *bedrockReasoning:
			reasoned = true
			if b.Signature == "" {
				b.Signature = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("sig:%s:%d", f.nextID("r"), utf8.RuneCountInString(b.Text))))
			}
			f.mu.Lock()
			f.signatures[b.Text] = b.Signature
			f.mu.Unlock()
			out = append(out, map[string]any{"reasoningContent": map[string]any{"reasoningText": map[string]any{"text": b.Text, "signature": b.Signature}}})
		case *bedrockToolUse:
			tools = true
			if b.ToolUseID == "" {
				b.ToolUseID = f.nextID("tooluse_")
			}
			out = append(out, map[string]any{"toolUse": map[string]any{"toolUseId": b.ToolUseID, "name": b.Name, "input": b.Input}})
		}
	}
	if reasoned {
		f.mu.Lock()
		for _, b := range turn.Blocks {
			if tu, ok := b.(*bedrockToolUse); ok {
				f.reasonedTools[tu.ToolUseID] = true
			}
		}
		f.mu.Unlock()
	}
	if turn.StopReason == "" {
		turn.StopReason = "end_turn"
		if tools {
			turn.StopReason = "tool_use"
		}
	}
	return out
}

// Recomputes a SigV4 signature with the known secret of its access key and returns the credential scope
func bedrockVerifySigV4(r *http.Request, body []byte, secrets map[string]string) (map[string]string, error) {
	auth := r.Header.Get("Authorization")
	m := bedrockFakeAuthRe.FindStringSubmatch(auth)
	if m == nil {
		return nil, &bedrockRejection{"MissingAuthenticationTokenException", fmt.Sprintf("Missing or malformed SigV4 Authorization: %q", auth)}
	}
	scope := map[string]string{"key": m[1], "date": m[2], "region": m[3], "service": m[4], "signed": m[5], "sig": m[6]}
	secret, ok := secrets[m[1]]
	if !ok || m[4] != "bedrock" {
		return nil, &bedrockRejection{"UnrecognizedClientException", "The security token included in the request is invalid."}
	}
	only := map[string]bool{}
	for _, h := range strings.Split(m[5], ";") {
		only[h] = true
	}
	ts, err := time.Parse(sigV4TimeFormat, r.Header.Get("X-Amz-Date"))
	if err != nil {
		return nil, &bedrockRejection{"IncompleteSignatureException", "X-Amz-Date is missing or malformed"}
	}
	clone := r.Clone(r.Context())
	if clone.URL.Host == "" {
		clone.URL.Host = r.Host
	}
	got := sigV4Compute(clone, sigV4PayloadHash(clone, body), awsCredentials{AccessKeyID: m[1], SecretAccessKey: secret}, "bedrock", m[3], ts, only)
	if got.Signature != m[6] {
		return nil, &bedrockRejection{"InvalidSignatureException", "The request signature we calculated does not match the signature you provided."}
	}
	return scope, nil
}

// HTTP status of an AWS error code, signature and token failures answering 403
func bedrockFakeErrorStatus(model *bedrockServiceModel, code string) int {
	if s, ok := model.Errors[code]; ok {
		return s
	}
	if strings.Contains(code, "Signature") || strings.Contains(code, "Token") {
		return 403
	}
	return 400
}

func (f *bedrockFake) writeError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-ErrorType", code+":http://internal.amazon.com/coral/com.amazon.bedrock/")
	w.Header().Set("x-amzn-RequestId", f.nextID("req-"))
	w.WriteHeader(bedrockFakeErrorStatus(bedrockModel, code))
	data, _ := json.Marshal(map[string]string{"message": message})
	_, _ = w.Write(data)
}

func (f *bedrockFake) writeJSON(w http.ResponseWriter, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-RequestId", f.nextID("req-"))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// Authenticates, validates, records and answers one call
func (f *bedrockFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	headers := map[string]string{"host": r.Host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	rec := &bedrockFakeRecord{Path: r.URL.RequestURI(), Headers: headers, Raw: raw, T: time.Now()}
	route := bedrockFakeRouteRe.FindStringSubmatch(r.URL.EscapedPath())
	if route != nil {
		rec.Op = bedrockFakeOps[route[2]]
		rec.Model, _ = url.PathUnescape(route[1])
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(append([]byte(nil), bedrockOrEmptyJSON(raw)...)))
	dec.UseNumber()
	bodyErr := dec.Decode(&body)
	rec.Body = body
	if rej := f.admit(r, raw, rec, route != nil, bodyErr); rej != nil {
		rec.Rejected = rej.Code + ": " + rej.Message
		f.record(rec)
		f.writeError(w, rej.Code, rej.Message)
		return
	}
	f.record(rec)
	reply := f.responder(rec)
	rec.Reply = strings.TrimPrefix(fmt.Sprintf("%T", reply), "*provider.")
	var turn *bedrockTurn
	switch rp := reply.(type) {
	case *bedrockHTTPError:
		f.writeError(w, rp.Code, rp.Message)
		return
	case *bedrockTurn:
		turn = rp
	case *bedrockStreamException:
		turn = rp.Turn
	case *bedrockDrop:
		turn = rp.Turn
	}
	blocks := f.materialize(turn)
	switch rec.Op {
	case "Converse":
		rec.Emitted = blocks
		f.writeJSON(w, map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": blocks}},
			"stopReason": turn.StopReason, "usage": bedrockFakeUsage(turn), "metrics": map[string]any{"latencyMs": 9}})
	case "ConverseStream":
		rec.Emitted = blocks
		f.stream(w, reply, f.converseFrames(blocks, turn))
	case "CountTokens":
		f.writeJSON(w, map[string]any{"inputTokens": turn.InputTokens})
	case "InvokeModel":
		rec.Emitted = bedrockAnthropicBlocks(blocks)
		f.writeJSON(w, bedrockAnthropicMessage(f.nextID("msg_bdrk_"), rec, turn, rec.Emitted))
	case "InvokeModelWithResponseStream":
		rec.Emitted = bedrockAnthropicBlocks(blocks)
		f.stream(w, reply, f.invokeFrames(rec, turn, rec.Emitted))
	}
}

func bedrockOrEmptyJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// Route, auth, shape and conversation checks in the order Bedrock applies them
func (f *bedrockFake) admit(r *http.Request, raw []byte, rec *bedrockFakeRecord, routed bool, bodyErr error) *bedrockRejection {
	if !routed {
		return &bedrockRejection{"UnknownOperationException", "No route for " + rec.Path}
	}
	if auth := r.Header.Get("Authorization"); f.bearer != "" && strings.HasPrefix(auth, "Bearer ") {
		if strings.TrimPrefix(auth, "Bearer ") != f.bearer {
			return &bedrockRejection{"UnrecognizedClientException", "The security token included in the request is invalid."}
		}
		rec.Auth = map[string]string{"bearer": f.bearer}
	} else {
		scope, err := bedrockVerifySigV4(r, raw, map[string]string{bedrockFakeAccessKey: bedrockFakeSecretKey})
		if err != nil {
			return err.(*bedrockRejection)
		}
		rec.Auth = scope
	}
	if bodyErr != nil || rec.Body == nil {
		return &bedrockRejection{"SerializationException", "Request body is not a JSON object"}
	}
	switch rec.Op {
	case "Converse", "ConverseStream":
		if rej := bedrockValidateShape(bedrockModel, rec.Op, rec.Model, rec.Body); rej != nil {
			return rej
		}
		f.mu.Lock()
		sigs := make(map[string]string, len(f.signatures))
		for k, v := range f.signatures {
			sigs[k] = v
		}
		tools := make(map[string]bool, len(f.reasonedTools))
		for k := range f.reasonedTools {
			tools[k] = true
		}
		f.mu.Unlock()
		return bedrockValidateConversation(rec.Body, sigs, tools)
	case "CountTokens":
		in, _ := rec.Body["input"].(map[string]any)
		im, _ := in["invokeModel"].(map[string]any)
		encoded, ok := im["body"].(string)
		if !ok {
			return &bedrockRejection{"ValidationException", "1 validation error detected: Value null at 'input.invokeModel.body' failed to satisfy constraint: Member must not be null"}
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return &bedrockRejection{"ValidationException", "Invalid base64 blob for Body"}
		}
		var inner map[string]any
		if err := json.Unmarshal(data, &inner); err != nil {
			return &bedrockRejection{"ValidationException", "Malformed input request, please reformat your input and try again."}
		}
		return bedrockValidateInvokeBody(inner)
	}
	return bedrockValidateInvokeBody(rec.Body)
}

// Messages body rules InvokeModel enforces for Anthropic models
func bedrockValidateInvokeBody(body map[string]any) *bedrockRejection {
	for _, key := range []string{"model", "stream"} {
		if _, has := body[key]; has {
			return &bedrockRejection{"ValidationException", "Malformed input request: #: extraneous key [" + key + "] is not permitted, please reformat your input and try again."}
		}
	}
	for _, key := range []string{"anthropic_version", "max_tokens", "messages"} {
		if _, has := body[key]; !has {
			return &bedrockRejection{"ValidationException", "Malformed input request: #: required key [" + key + "] not found, please reformat your input and try again."}
		}
	}
	return nil
}

func bedrockFakeUsage(turn *bedrockTurn) map[string]any {
	return map[string]any{"inputTokens": turn.InputTokens, "outputTokens": turn.OutputTokens, "totalTokens": turn.InputTokens + turn.OutputTokens}
}

// Splits text into rune pieces of the given size, one empty piece for empty text
func bedrockPieces(text string, size int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	var out []string
	for i := 0; i < len(runes); i += size {
		out = append(out, string(runes[i:min(i+size, len(runes))]))
	}
	return out
}

// Smallest text chunk size of the turn, twelve when it has no text block
func bedrockTurnChunk(turn *bedrockTurn) int {
	chunk := 0
	for _, b := range turn.Blocks {
		if tb, ok := b.(*bedrockText); ok && tb.Chunk > 0 && (chunk == 0 || tb.Chunk < chunk) {
			chunk = tb.Chunk
		}
	}
	if chunk == 0 {
		return 12
	}
	return chunk
}

// ConverseStream frames of a turn, text blocks without a start event as on AWS
func (f *bedrockFake) converseFrames(blocks []map[string]any, turn *bedrockTurn) [][]byte {
	chunk := bedrockTurnChunk(turn)
	type ev struct {
		name string
		body map[string]any
	}
	events := []ev{{"messageStart", map[string]any{"role": "assistant"}}}
	for i, block := range blocks {
		var deltas []map[string]any
		switch {
		case block["toolUse"] != nil:
			tu := block["toolUse"].(map[string]any)
			events = append(events, ev{"contentBlockStart", map[string]any{"contentBlockIndex": i, "start": map[string]any{
				"toolUse": map[string]any{"toolUseId": tu["toolUseId"], "name": tu["name"]}}}})
			input, _ := json.Marshal(tu["input"])
			for _, p := range bedrockPieces(bedrockPyJSONSpacing(string(input)), chunk) {
				deltas = append(deltas, map[string]any{"toolUse": map[string]any{"input": p}})
			}
		case block["reasoningContent"] != nil:
			rt := block["reasoningContent"].(map[string]any)["reasoningText"].(map[string]any)
			for _, p := range bedrockPieces(rt["text"].(string), chunk) {
				deltas = append(deltas, map[string]any{"reasoningContent": map[string]any{"text": p}})
			}
			deltas = append(deltas, map[string]any{"reasoningContent": map[string]any{"signature": rt["signature"]}})
		default:
			for _, p := range bedrockPieces(block["text"].(string), chunk) {
				deltas = append(deltas, map[string]any{"text": p})
			}
		}
		for _, d := range deltas {
			events = append(events, ev{"contentBlockDelta", map[string]any{"contentBlockIndex": i, "delta": d}})
		}
		events = append(events, ev{"contentBlockStop", map[string]any{"contentBlockIndex": i}})
	}
	events = append(events, ev{"messageStop", map[string]any{"stopReason": turn.StopReason}},
		ev{"metadata", map[string]any{"usage": bedrockFakeUsage(turn), "metrics": map[string]any{"latencyMs": 7}}})
	frames := make([][]byte, 0, len(events))
	for _, e := range events {
		frames = append(frames, f.eventFrame(e.name, e.body))
	}
	return frames
}

// Event frame of a ConverseStream member, failing the test when the fake built an invalid event
func (f *bedrockFake) eventFrame(eventType string, body map[string]any) []byte {
	stream := bedrockModel.Shapes[bedrockModel.Operations["ConverseStream"]["stream"]]
	members, _ := stream["members"].(map[string]any)
	ref, _ := members[eventType].(map[string]any)
	data, _ := json.Marshal(body)
	var wire any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&wire)
	var errs []string
	bedrockValidateValue(bedrockModel, stringOf(ref["shape"]), wire, eventType, &errs)
	if len(errs) > 0 || ref == nil {
		f.t.Errorf("fake built an invalid %s event: %s", eventType, strings.Join(errs, "\n"))
	}
	frame, err := awsEncodeEventStreamMessage(awsEventHeaders(eventType, "application/json"), data)
	if err != nil {
		f.t.Errorf("encode %s frame: %v", eventType, err)
	}
	return frame
}

// Python json.dumps separators applied to compact Go JSON
func bedrockPyJSONSpacing(compact string) string {
	var b strings.Builder
	inStr, esc := false, false
	for _, c := range compact {
		b.WriteRune(c)
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && (c == ',' || c == ':'):
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// Anthropic Messages blocks of the Converse blocks of a turn
func bedrockAnthropicBlocks(blocks []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block["toolUse"] != nil:
			tu := block["toolUse"].(map[string]any)
			out = append(out, map[string]any{"type": "tool_use", "id": tu["toolUseId"], "name": tu["name"], "input": tu["input"]})
		case block["reasoningContent"] != nil:
			rt := block["reasoningContent"].(map[string]any)["reasoningText"].(map[string]any)
			out = append(out, map[string]any{"type": "thinking", "thinking": rt["text"], "signature": rt["signature"]})
		default:
			out = append(out, map[string]any{"type": "text", "text": block["text"]})
		}
	}
	return out
}

// InvokeModel Messages reply, flagged when the turn names a guardrail action
func bedrockAnthropicMessage(id string, rec *bedrockFakeRecord, turn *bedrockTurn, blocks []map[string]any) map[string]any {
	msg := map[string]any{"id": id, "type": "message", "role": "assistant",
		"model": rec.Model, "content": blocks, "stop_reason": turn.StopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": turn.InputTokens, "output_tokens": turn.OutputTokens}}
	if turn.GuardrailAction != "" {
		msg[BedrockGuardrailActionField] = turn.GuardrailAction
	}
	return msg
}

// InvokeModelWithResponseStream chunk frames carrying the Anthropic stream events of a turn
func (f *bedrockFake) invokeFrames(rec *bedrockFakeRecord, turn *bedrockTurn, blocks []map[string]any) [][]byte {
	chunk := bedrockTurnChunk(turn)
	start := bedrockAnthropicMessage(f.nextID("msg_bdrk_"), rec, turn, []map[string]any{})
	start["stop_reason"] = nil
	start["usage"] = map[string]any{"input_tokens": turn.InputTokens, "output_tokens": 1}
	delete(start, BedrockGuardrailActionField)
	events := []map[string]any{{"type": "message_start", "message": start}}
	for i, b := range blocks {
		var open map[string]any
		var deltas []map[string]any
		switch b["type"] {
		case "tool_use":
			open = map[string]any{"type": "tool_use", "id": b["id"], "name": b["name"], "input": map[string]any{}}
			input, _ := json.Marshal(b["input"])
			for _, p := range bedrockPieces(string(input), chunk) {
				deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": p})
			}
		case "thinking":
			open = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
			for _, p := range bedrockPieces(b["thinking"].(string), chunk) {
				deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": p})
			}
			deltas = append(deltas, map[string]any{"type": "signature_delta", "signature": b["signature"]})
		default:
			open = map[string]any{"type": "text", "text": ""}
			for _, p := range bedrockPieces(b["text"].(string), chunk) {
				deltas = append(deltas, map[string]any{"type": "text_delta", "text": p})
			}
		}
		events = append(events, map[string]any{"type": "content_block_start", "index": i, "content_block": open})
		for _, d := range deltas {
			events = append(events, map[string]any{"type": "content_block_delta", "index": i, "delta": d})
		}
		events = append(events, map[string]any{"type": "content_block_stop", "index": i})
	}
	events = append(events,
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": turn.StopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": turn.OutputTokens}},
		map[string]any{"type": "message_stop", "amazon-bedrock-invocationMetrics": map[string]any{"inputTokenCount": turn.InputTokens,
			"outputTokenCount": turn.OutputTokens, "invocationLatency": 7, "firstByteLatency": 3}})
	frames := make([][]byte, 0, len(events))
	for _, e := range events {
		frame, err := bedrockInvokeChunkFrame(e)
		if err != nil {
			f.t.Errorf("encode invoke chunk: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// Streams frames up to the reply's cut, then an exception frame, a clean end or an aborted connection
func (f *bedrockFake) stream(w http.ResponseWriter, reply bedrockReply, frames [][]byte) {
	cut := len(frames)
	switch rp := reply.(type) {
	case *bedrockStreamException:
		cut = min(rp.After, len(frames))
	case *bedrockDrop:
		cut = min(rp.After, len(frames))
	}
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.Header().Set("x-amzn-RequestId", f.nextID("req-"))
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	for _, frame := range frames[:cut] {
		_, _ = w.Write(frame)
		flusher.Flush()
	}
	switch rp := reply.(type) {
	case *bedrockDrop:
		if !rp.Clean {
			panic(http.ErrAbortHandler)
		}
	case *bedrockStreamException:
		frame, err := awsEventStreamExceptionFrame(rp.Exception, rp.Message)
		if err != nil {
			f.t.Errorf("encode exception frame: %v", err)
		}
		_, _ = w.Write(frame)
		flusher.Flush()
	}
}

// ---- botocore ParamValidator over the Converse input shapes ----

// Validates a Converse body plus the URI model id against the operation's input shape
func bedrockValidateShape(model *bedrockServiceModel, op, modelID string, body map[string]any) *bedrockRejection {
	params := map[string]any{"modelId": modelID}
	for k, v := range body {
		params[k] = v
	}
	shapeName := model.Operations[op]["input"]
	converted, rej := bedrockWireToParams(model, shapeName, params)
	if rej != nil {
		return rej
	}
	var errs []string
	bedrockValidateValue(model, shapeName, converted, "", &errs)
	if len(errs) > 0 {
		return &bedrockRejection{"ValidationException", "Parameter validation failed:\n" + strings.Join(errs, "\n")}
	}
	return nil
}

// Rest JSON wire value as a botocore param, base64 blobs decoded
func bedrockWireToParams(model *bedrockServiceModel, shapeName string, value any) (any, *bedrockRejection) {
	shape := model.Shapes[shapeName]
	switch shape["type"] {
	case "blob":
		s, ok := value.(string)
		if !ok {
			return value, nil
		}
		if strings.ContainsAny(s, "\r\n") {
			return nil, &bedrockRejection{"ValidationException", "Invalid base64 blob for " + shapeName}
		}
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, &bedrockRejection{"ValidationException", "Invalid base64 blob for " + shapeName}
		}
		return data, nil
	case "structure":
		m, ok := value.(map[string]any)
		if !ok || shape["document"] == true {
			return value, nil
		}
		members, _ := shape["members"].(map[string]any)
		out := make(map[string]any, len(m))
		for k, v := range m {
			ref, known := members[k].(map[string]any)
			if !known {
				out[k] = v
				continue
			}
			cv, rej := bedrockWireToParams(model, stringOf(ref["shape"]), v)
			if rej != nil {
				return nil, rej
			}
			out[k] = cv
		}
		return out, nil
	case "list":
		l, ok := value.([]any)
		if !ok {
			return value, nil
		}
		member := stringOf(shape["member"].(map[string]any)["shape"])
		out := make([]any, len(l))
		for i, v := range l {
			cv, rej := bedrockWireToParams(model, member, v)
			if rej != nil {
				return nil, rej
			}
			out[i] = cv
		}
		return out, nil
	case "map":
		m, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		vshape := stringOf(shape["value"].(map[string]any)["shape"])
		out := make(map[string]any, len(m))
		for k, v := range m {
			cv, rej := bedrockWireToParams(model, vshape, v)
			if rej != nil {
				return nil, rej
			}
			out[k] = cv
		}
		return out, nil
	}
	return value, nil
}

// Python type name botocore reports for an invalid parameter
func bedrockPyType(v any) string {
	switch v := v.(type) {
	case nil:
		return "<class 'NoneType'>"
	case bool:
		return "<class 'bool'>"
	case string:
		return "<class 'str'>"
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return "<class 'float'>"
		}
		return "<class 'int'>"
	case []any:
		return "<class 'list'>"
	case map[string]any:
		return "<class 'dict'>"
	case []byte:
		return "<class 'bytes'>"
	}
	return fmt.Sprintf("<class '%T'>", v)
}

// Python repr botocore prints for an invalid value
func bedrockPyRepr(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return "'" + v + "'"
	case json.Number:
		return string(v)
	}
	data, _ := json.Marshal(v)
	return string(data)
}

// Appends botocore ParamValidator errors for the value against the named shape
func bedrockValidateValue(model *bedrockServiceModel, shapeName string, value any, name string, errs *[]string) {
	shape := model.Shapes[shapeName]
	label := name
	if label == "" {
		label = "input"
	}
	invalidType := func(valid string) {
		*errs = append(*errs, fmt.Sprintf("Invalid type for parameter %s, value: %s, type: %s, valid types: %s", label, bedrockPyRepr(value), bedrockPyType(value), valid))
	}
	minOf := func() (int64, bool) {
		n, ok := anyInt(shape["min"])
		return int64(n), ok
	}
	join := func(child string) string {
		if name == "" {
			return child
		}
		return name + "." + child
	}
	switch shape["type"] {
	case "structure":
		if shape["document"] == true {
			return
		}
		m, ok := value.(map[string]any)
		if !ok {
			invalidType("<class 'dict'>")
			return
		}
		members, _ := shape["members"].(map[string]any)
		names := make([]string, 0, len(members))
		for k := range members {
			names = append(names, k)
		}
		sort.Strings(names)
		if req, _ := shape["required"].([]any); len(req) > 0 {
			for _, r := range req {
				if _, has := m[stringOf(r)]; !has {
					*errs = append(*errs, fmt.Sprintf("Missing required parameter in %s: %q", label, stringOf(r)))
				}
			}
		}
		if shape["union"] == true {
			if len(m) == 0 {
				*errs = append(*errs, fmt.Sprintf("Must set one of the following keys for tagged union structure %s: %s.", label, strings.Join(names, ", ")))
			} else if len(m) > 1 {
				*errs = append(*errs, fmt.Sprintf("Invalid number of parameters set for tagged union structure %s. Can only set one of the following keys: %s.", label, strings.Join(names, ", ")))
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ref, known := members[k].(map[string]any)
			if !known {
				*errs = append(*errs, fmt.Sprintf("Unknown parameter in %s: %q, must be one of: %s", label, k, strings.Join(names, ", ")))
				continue
			}
			bedrockValidateValue(model, stringOf(ref["shape"]), m[k], join(k), errs)
		}
	case "string":
		s, ok := value.(string)
		if !ok {
			invalidType("<class 'str'>")
			return
		}
		if lo, ok := minOf(); ok && int64(utf8.RuneCountInString(s)) < lo {
			*errs = append(*errs, fmt.Sprintf("Invalid length for parameter %s, value: %d, valid min length: %d", label, utf8.RuneCountInString(s), lo))
		}
	case "integer", "long":
		n, ok := value.(json.Number)
		if !ok || strings.ContainsAny(string(n), ".eE") {
			invalidType("<class 'int'>")
			return
		}
		v, _ := n.Int64()
		if lo, ok := minOf(); ok && v < lo {
			*errs = append(*errs, fmt.Sprintf("Invalid value for parameter %s, value: %d, valid min value: %d", label, v, lo))
		}
	case "float", "double":
		if _, ok := value.(json.Number); !ok {
			invalidType("<class 'float'>, <class 'decimal.Decimal'>, <class 'int'>")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			invalidType("<class 'bool'>")
		}
	case "blob":
		if _, ok := value.([]byte); !ok {
			invalidType("<class 'bytes'>, <class 'bytearray'>, file-like object")
		}
	case "list":
		l, ok := value.([]any)
		if !ok {
			invalidType("<class 'list'>, <class 'tuple'>")
			return
		}
		if lo, ok := minOf(); ok && int64(len(l)) < lo {
			*errs = append(*errs, fmt.Sprintf("Invalid length for parameter %s, value: %d, valid min length: %d", label, len(l), lo))
		}
		member := stringOf(shape["member"].(map[string]any)["shape"])
		for i, v := range l {
			bedrockValidateValue(model, member, v, fmt.Sprintf("%s[%d]", name, i), errs)
		}
	case "map":
		m, ok := value.(map[string]any)
		if !ok {
			invalidType("<class 'dict'>")
			return
		}
		kshape := stringOf(shape["key"].(map[string]any)["shape"])
		vshape := stringOf(shape["value"].(map[string]any)["shape"])
		for k, v := range m {
			bedrockValidateValue(model, kshape, k, label, errs)
			bedrockValidateValue(model, vshape, v, label+"."+k, errs)
		}
	}
}

// ---- Converse conversation rules beyond the schema ----

func bedrockToolIDs(content []any, kind string) []string {
	var out []string
	for _, b := range content {
		if bm, ok := b.(map[string]any); ok {
			if inner, ok := bm[kind].(map[string]any); ok {
				out = append(out, stringOf(inner["toolUseId"]))
			}
		}
	}
	return out
}

// Converse rejections with the real ValidationException texts
func bedrockValidateConversation(body map[string]any, signatures map[string]string, reasonedTools map[string]bool) *bedrockRejection {
	messages, _ := body["messages"].([]any)
	first, _ := asMap(bedrockFirstOrNil(messages))
	if len(messages) == 0 || stringOf(first["role"]) != "user" {
		return &bedrockRejection{"ValidationException", "A conversation must start with a user message. Try again with a conversation that starts with a user message."}
	}
	for i, raw := range messages {
		msg, _ := raw.(map[string]any)
		content, _ := msg["content"].([]any)
		var prevContent []any
		if i > 0 {
			prev, _ := messages[i-1].(map[string]any)
			if stringOf(msg["role"]) == stringOf(prev["role"]) {
				return &bedrockRejection{"ValidationException", "A conversation must alternate between user and assistant roles. Make sure the conversation alternates between user and assistant roles and try again."}
			}
			prevContent, _ = prev["content"].([]any)
		}
		for j, rb := range content {
			block, _ := rb.(map[string]any)
			if text, has := block["text"]; has && strings.TrimSpace(stringOf(text)) == "" {
				return &bedrockRejection{"ValidationException", fmt.Sprintf("The text field in the ContentBlock object at messages.%d.content.%d is blank. Add text to the text field, and try again.", i, j)}
			}
			if rej := bedrockCheckReasoning(block["reasoningContent"], signatures, fmt.Sprintf("messages.%d.content.%d", i, j)); rej != nil {
				return rej
			}
		}
		var expected []string
		if i > 0 && stringOf(msg["role"]) == "user" {
			expected = bedrockToolIDs(prevContent, "toolUse")
		}
		got := bedrockToolIDs(content, "toolResult")
		se, sg := append([]string(nil), expected...), append([]string(nil), got...)
		sort.Strings(se)
		sort.Strings(sg)
		if strings.Join(se, "\x00") != strings.Join(sg, "\x00") || len(se) != len(sg) {
			return &bedrockRejection{"ValidationException", fmt.Sprintf("Expected toolResult blocks at messages.%d.content for the following Ids: %s; got %s", i, bedrockOrNone(expected), bedrockOrNone(got))}
		}
	}
	return bedrockCheckFinalAssistantThinking(messages, reasonedTools)
}

func bedrockFirstOrNil(l []any) any {
	if len(l) == 0 {
		return nil
	}
	return l[0]
}

func bedrockOrNone(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}

// While a tool loop is open the final assistant turn must start with its issued reasoning block
func bedrockCheckFinalAssistantThinking(messages []any, reasonedTools map[string]bool) *bedrockRejection {
	if len(messages) < 2 {
		return nil
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	lastContent, _ := last["content"].([]any)
	if len(bedrockToolIDs(lastContent, "toolResult")) == 0 {
		return nil
	}
	finalMsg, _ := messages[len(messages)-2].(map[string]any)
	final, _ := finalMsg["content"].([]any)
	reasoned := false
	for _, id := range bedrockToolIDs(final, "toolUse") {
		reasoned = reasoned || reasonedTools[id]
	}
	if !reasoned || len(final) == 0 {
		return nil
	}
	head, _ := final[0].(map[string]any)
	if _, has := head["reasoningContent"]; has {
		return nil
	}
	firstKey := "?"
	keys := make([]string, 0, len(head))
	for k := range head {
		keys = append(keys, k)
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		firstKey = keys[0]
	}
	return &bedrockRejection{"ValidationException", fmt.Sprintf("messages.%d.content.0.type: Expected `thinking` or `redacted_thinking`, but found `%s`. "+
		"When `thinking` is enabled, a final `assistant` message must start with a thinking block (preceeding the lastmost set of `tool_use` and `tool_result` blocks).", len(messages)-2, firstKey)}
}

// A replayed reasoning text must carry exactly the signature the fake issued for it
func bedrockCheckReasoning(reasoning any, signatures map[string]string, where string) *bedrockRejection {
	rc, _ := reasoning.(map[string]any)
	textBlock, _ := rc["reasoningText"].(map[string]any)
	if len(textBlock) == 0 {
		return nil
	}
	issued, ok := signatures[stringOf(textBlock["text"])]
	if !ok || stringOf(textBlock["signature"]) != issued {
		return &bedrockRejection{"ValidationException", where + ": The reasoning block signature is missing or invalid. Reasoning content must be passed back unmodified with its signature."}
	}
	return nil
}

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Input rounds one request may take before the server is taken to loop
const maxInputRounds = 16

// Retry interval of a request waiting on a browser step whose finish the server may never report
var urlElicitationRetryEvery = 10 * time.Second

// CodeURLElicitationRequired is the 2025-11-25 error a request gets until a URL mode elicitation is finished
const CodeURLElicitationRequired = -32042

// URLElicitationError is a request held up by browser steps the user declined or has not finished in time
type URLElicitationError struct {
	Server  string
	Method  string
	URLs    []string
	Outcome string
	Wait    time.Duration
}

func (e *URLElicitationError) Error() string {
	links := strings.Join(e.URLs, ", ")
	switch e.Outcome {
	case "pending":
		return fmt.Sprintf("MCP server '%s' needs the browser step at %s finished before it can answer %s. The user accepted it, but the server has not reported it finished within %ds. Call it again once the user says the page is done.",
			e.Server, links, e.Method, int(e.Wait.Seconds()))
	case "cancel":
		return fmt.Sprintf("MCP server '%s' needs the browser step at %s before it can answer %s, and the user dismissed the request to open it.", e.Server, links, e.Method)
	}
	return fmt.Sprintf("MCP server '%s' needs the browser step at %s before it can answer %s, and the user declined to open it.", e.Server, links, e.Method)
}

// Version of a session on a per request revision, false on a handshake revision or before negotiation
func (s *Session) modernEra() (string, bool) {
	v := s.ProtocolVersion()
	return v, v != "" && !supportedVersion(v)
}

// Adds the per request protocol version, client identity and capabilities to a request's _meta
func (s *Session) stampMeta(msg *Message, version string) error {
	params := map[string]json.RawMessage{}
	if len(msg.Params) > 0 && string(msg.Params) != "null" {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("stamp %s params: %w", msg.Method, err)
		}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := params["_meta"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("stamp %s _meta: %w", msg.Method, err)
		}
	}
	for key, value := range map[string]any{
		"io.modelcontextprotocol/protocolVersion":    version,
		"io.modelcontextprotocol/clientInfo":         Implementation{Name: ClientName, Version: ClientVersion},
		"io.modelcontextprotocol/clientCapabilities": s.capabilities(),
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		meta[key] = raw
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	params["_meta"] = rawMeta
	msg.Params, err = json.Marshal(params)
	return err
}

// One request under a fresh id, opting into progress with the id as token, answering the raw result
func (s *Session) roundTrip(ctx context.Context, method string, params map[string]any, progress ProgressFunc) (json.RawMessage, error) {
	id := nextID()
	body := make(map[string]any, len(params)+1)
	for k, v := range params {
		body[k] = v
	}
	if progress != nil {
		body["_meta"] = map[string]any{"progressToken": json.RawMessage(id)}
		key := IDKey(id)
		s.mu.Lock()
		s.progress[key] = func(p ProgressParams) { progress(p.Progress, p.Total, p.Message) }
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.progress, key)
			s.mu.Unlock()
		}()
	}
	raw, err := marshalParams(body)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err := s.request(ctx, &Message{JSONRPC: "2.0", ID: id, Method: method, Params: raw}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Sends a request a server may answer with input required or a URL elicitation required error, fulfilling each and retrying to the final result
func (s *Session) callWithInputs(ctx context.Context, method string, params map[string]any, progress ProgressFunc, out any) error {
	accepted := map[string]bool{}
	var deadline time.Time
	for round := 0; ; round++ {
		raw, err := s.roundTrip(ctx, method, params, progress)
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == CodeURLElicitationRequired {
			if err := s.awaitURLElicitations(ctx, method, rpcErr, accepted, &deadline); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var head struct {
			ResultType string `json:"resultType"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &head); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		switch head.ResultType {
		case "", "complete":
			if out == nil || len(raw) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
			return nil
		case "input_required":
		default:
			return fmt.Errorf("MCP server '%s' answered %s with resultType %q, which this client does not know", s.opts.Name, method, head.ResultType)
		}
		if round+1 >= maxInputRounds {
			return fmt.Errorf("MCP server '%s' still asked for more input after %d rounds of %s", s.opts.Name, maxInputRounds, method)
		}
		if params, err = s.fulfillInputs(ctx, method, raw, params); err != nil {
			return err
		}
	}
}

// Answers every input request of an input required result, returning the retry params with the answers and the echoed state
func (s *Session) fulfillInputs(ctx context.Context, method string, raw json.RawMessage, params map[string]any) (map[string]any, error) {
	var asked struct {
		InputRequests map[string]struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		} `json:"inputRequests"`
		RequestState *string `json:"requestState"`
	}
	if err := json.Unmarshal(raw, &asked); err != nil {
		return nil, fmt.Errorf("decode %s input request: %w", method, err)
	}
	if len(asked.InputRequests) == 0 && asked.RequestState == nil {
		return nil, fmt.Errorf("MCP server '%s' answered %s with input_required carrying neither inputRequests nor requestState", s.opts.Name, method)
	}
	responses := map[string]any{}
	keys := make([]string, 0, len(asked.InputRequests))
	for key := range asked.InputRequests {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		req := asked.InputRequests[key]
		answer, err := s.answerInput(ctx, req.Method, req.Params)
		if err != nil {
			return nil, fmt.Errorf("MCP server '%s' input request %q of %s: %w", s.opts.Name, key, method, err)
		}
		responses[key] = answer
	}
	next := map[string]any{}
	for k, v := range params {
		if k != "inputResponses" && k != "requestState" {
			next[k] = v
		}
	}
	if len(asked.InputRequests) > 0 {
		next["inputResponses"] = responses
	}
	if asked.RequestState != nil {
		next["requestState"] = *asked.RequestState
	}
	return next, nil
}

// Result of one server input request through the elicitor or the sampler this client declared
func (s *Session) answerInput(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case methodElicitationCreate:
		if s.opts.Elicitor == nil {
			return nil, errors.New("elicitation/create was asked although elicitation is turned off for this server, so it was never declared")
		}
		var p ElicitParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("decode elicitation/create params: %w", err)
		}
		res, rpcErr := s.opts.Elicitor.Elicit(ctx, &p)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return res, nil
	case methodSamplingCreateMessage:
		if s.opts.Sampler == nil {
			return nil, errors.New("sampling/createMessage was asked although sampling is turned off for this server, so it was never declared")
		}
		var p CreateMessageParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("decode sampling/createMessage params: %w", err)
		}
		res, rpcErr := s.opts.Sampler.CreateMessage(ctx, &p)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return res, nil
	}
	return nil, fmt.Errorf("%s was asked, which this client does not declare", method)
}

// Puts each new elicitation of a URL elicitation required error to the user, then waits for the server's finish report or the retry interval
func (s *Session) awaitURLElicitations(ctx context.Context, method string, rpcErr *RPCError, accepted map[string]bool, deadline *time.Time) error {
	var data struct {
		Elicitations []ElicitParams `json:"elicitations"`
	}
	if len(rpcErr.Data) == 0 || json.Unmarshal(rpcErr.Data, &data) != nil || len(data.Elicitations) == 0 {
		return rpcErr
	}
	tracker, ok := s.opts.Elicitor.(urlElicitationTracker)
	if !ok {
		return fmt.Errorf("MCP server '%s' requires a browser step although elicitation is turned off for this server: %w", s.opts.Name, rpcErr)
	}
	var waits []<-chan struct{}
	var links []string
	for i := range data.Elicitations {
		e := &data.Elicitations[i]
		links = append(links, e.URL)
		if e.Mode != "url" || e.ElicitationID == "" {
			return fmt.Errorf("MCP server '%s' listed an elicitation that is not url mode with an elicitationId: %w", s.opts.Name, rpcErr)
		}
		if accepted[e.ElicitationID] && tracker.ElicitationDone(e.ElicitationID) == nil {
			accepted[e.ElicitationID] = false
		}
		if !accepted[e.ElicitationID] {
			res, perr := s.opts.Elicitor.Elicit(ctx, e)
			if perr != nil {
				return perr
			}
			if res.Action != "accept" {
				return &URLElicitationError{Server: s.opts.Name, Method: method, URLs: []string{e.URL}, Outcome: res.Action}
			}
			accepted[e.ElicitationID] = true
		}
		if done := tracker.ElicitationDone(e.ElicitationID); done != nil {
			waits = append(waits, done)
		}
	}
	wait := tracker.ElicitationWait()
	if deadline.IsZero() {
		*deadline = time.Now().Add(wait)
	}
	timer := time.NewTimer(urlElicitationRetryEvery)
	defer timer.Stop()
	finished := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for _, done := range waits {
			select {
			case <-done:
			case <-stop:
				return
			}
		}
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-timer.C:
		if time.Now().After(*deadline) {
			return &URLElicitationError{Server: s.opts.Name, Method: method, URLs: links, Outcome: "pending", Wait: wait}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

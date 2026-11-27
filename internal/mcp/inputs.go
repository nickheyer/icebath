package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
)

// Input rounds one request may take before the server is taken to loop
const inputRounds = 16

// How often a request waiting on a browser step is tried again, the server maybe never reporting it finished
const urlRetryEvery = 10 * time.Second

// Request held up by browser steps the user declined, dismissed or did not finish in time
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

// Revision of a session on per request revisions, false on a handshake revision or before negotiation
func (s *Session) modern() (string, bool) {
	v := s.ProtocolVersion()
	return v, v != "" && !handshakeVersion(v)
}

// Adds the per request revision, client identity and capabilities to a request's _meta
func (s *Session) stamp(msg *Message, version string) error {
	params := map[string]json.RawMessage{}
	if len(msg.Params) > 0 && string(msg.Params) != "null" {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("Could not prepare the request %s: %w", msg.Method, err)
		}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := params["_meta"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("Could not prepare the request %s: %w", msg.Method, err)
		}
	}
	for key, value := range map[string]any{
		"io.modelcontextprotocol/protocolVersion":    version,
		"io.modelcontextprotocol/clientInfo":         Implementation{Name: ClientName, Version: provider.Version()},
		"io.modelcontextprotocol/clientCapabilities": s.capabilities(),
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		meta[key] = raw
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	params["_meta"] = raw
	msg.Params, err = json.Marshal(params)
	return err
}

// One request under a fresh id, asking for progress with the id as token when given a sink
func (s *Session) roundTrip(ctx context.Context, method string, params map[string]any, progress ProgressFunc) (json.RawMessage, error) {
	id := nextID()
	body := maps.Clone(params)
	if body == nil {
		body = map[string]any{}
	}
	if progress != nil {
		body["_meta"] = map[string]any{"progressToken": id}
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
	raw, err := encodeParams(body)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err := s.request(ctx, &Message{JSONRPC: "2.0", ID: id, Method: method, Params: raw}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// A request the server may answer with input required or URL elicitation required, each fulfilled and the request sent again until the
// final answer
func (s *Session) callWithInputs(ctx context.Context, method string, params map[string]any, progress ProgressFunc, out any) error {
	accepted := map[string]bool{}
	var deadline time.Time
	for round := 0; ; round++ {
		raw, err := s.roundTrip(ctx, method, params, progress)
		if rpc, ok := errors.AsType[*RPCError](err); ok && rpc.Code == CodeURLElicitationRequired {
			if err := s.awaitURLs(ctx, method, rpc, accepted, &deadline); err != nil {
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
				return fmt.Errorf("The answer of the server to %s cannot be read: %w", method, err)
			}
		}
		switch head.ResultType {
		case "", "complete":
			if out == nil || len(raw) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("The answer of the server to %s cannot be read: %w", method, err)
			}
			return nil
		case "input_required":
		default:
			return fmt.Errorf("The MCP server %s answered %s with a result of the kind “%s”, which icebath does not know.", s.opts.Name, method, head.ResultType)
		}
		if round+1 >= inputRounds {
			return fmt.Errorf("The MCP server %s still asked for more input after %d rounds of %s.", s.opts.Name, inputRounds, method)
		}
		if params, err = s.fulfill(ctx, method, raw, params); err != nil {
			return err
		}
	}
}

// Answers every input request of an input required answer, the params of the next round carrying the answers and the echoed state
func (s *Session) fulfill(ctx context.Context, method string, raw json.RawMessage, params map[string]any) (map[string]any, error) {
	var asked struct {
		Requests map[string]struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		} `json:"inputRequests"`
		State *string `json:"requestState"`
	}
	if err := json.Unmarshal(raw, &asked); err != nil {
		return nil, fmt.Errorf("The request of the server for input to %s cannot be read: %w", method, err)
	}
	if len(asked.Requests) == 0 && asked.State == nil {
		return nil, fmt.Errorf("The MCP server %s asked for input to %s without saying what it needs.", s.opts.Name, method)
	}
	responses := map[string]any{}
	for _, key := range slices.Sorted(maps.Keys(asked.Requests)) {
		request := asked.Requests[key]
		answer, err := s.input(ctx, request.Method, request.Params)
		if err != nil {
			return nil, fmt.Errorf("The MCP server %s asked for the input %s to %s in a form icebath cannot read: %w", s.opts.Name, key, method, err)
		}
		responses[key] = answer
	}
	next := map[string]any{}
	for k, v := range params {
		if k != "inputResponses" && k != "requestState" {
			next[k] = v
		}
	}
	if len(asked.Requests) > 0 {
		next["inputResponses"] = responses
	}
	if asked.State != nil {
		next["requestState"] = *asked.State
	}
	return next, nil
}

// Answer of one input request through the elicitor or the sampler this client declared
func (s *Session) input(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case MethodElicit:
		if s.opts.Elicitor == nil {
			return nil, errors.New("elicitation/create was asked although elicitation is turned off for this server, so it was never declared")
		}
		var p ElicitParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("decode elicitation/create params: %w", err)
		}
		result, failure := s.opts.Elicitor.Elicit(ctx, &p)
		if failure != nil {
			return nil, failure
		}
		return result, nil
	case MethodCreateMessage:
		if s.opts.Sampler == nil {
			return nil, errors.New("sampling/createMessage was asked although sampling is turned off for this server, so it was never declared")
		}
		var p CreateMessageParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("decode sampling/createMessage params: %w", err)
		}
		result, failure := s.opts.Sampler.CreateMessage(ctx, &p)
		if failure != nil {
			return nil, failure
		}
		return result, nil
	}
	return nil, fmt.Errorf("%s was asked, which this client does not declare", method)
}

// Puts each new browser step of a URL elicitation required error to the user, then waits for the server to report them finished, or for
// the retry interval
func (s *Session) awaitURLs(ctx context.Context, method string, rpc *RPCError, accepted map[string]bool, deadline *time.Time) error {
	var data struct {
		Elicitations []ElicitParams `json:"elicitations"`
	}
	if len(rpc.Data) == 0 || json.Unmarshal(rpc.Data, &data) != nil || len(data.Elicitations) == 0 {
		return rpc
	}
	tracker, ok := s.opts.Elicitor.(URLTracker)
	if !ok {
		return fmt.Errorf("The MCP server %s needs a step in the browser, but asking you is turned off for this server: %w", s.opts.Name, rpc)
	}
	var waits []<-chan struct{}
	var links []string
	for i := range data.Elicitations {
		e := &data.Elicitations[i]
		links = append(links, e.URL)
		if e.Mode != "url" || e.ElicitationID == "" {
			return fmt.Errorf("The MCP server %s asked for a step in the browser in a form icebath cannot use: %w", s.opts.Name, rpc)
		}
		if accepted[e.ElicitationID] && tracker.Done(e.ElicitationID) == nil {
			accepted[e.ElicitationID] = false
		}
		if !accepted[e.ElicitationID] {
			result, failure := s.opts.Elicitor.Elicit(ctx, e)
			if failure != nil {
				return failure
			}
			if result.Action != "accept" {
				return &URLElicitationError{Server: s.opts.Name, Method: method, URLs: []string{e.URL}, Outcome: result.Action}
			}
			accepted[e.ElicitationID] = true
		}
		if done := tracker.Done(e.ElicitationID); done != nil {
			waits = append(waits, done)
		}
	}
	wait := tracker.Wait()
	if deadline.IsZero() {
		*deadline = time.Now().Add(wait)
	}
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
	timer := time.NewTimer(urlRetryEvery)
	defer timer.Stop()
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

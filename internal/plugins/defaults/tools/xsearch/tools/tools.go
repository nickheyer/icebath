package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	xsearchconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/xsearch/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/web"
)

const (
	// Most handles one search may name
	maxHandles = 10
	// Fewest seconds a search may take
	minTimeout = 30
)

const description = "Search X (Twitter) posts, profiles and threads via xAI's built-in X Search. Read-only discovery only: use this for current " +
	"discussion, reactions, or claims on public X rather than general web pages. Do not use it to post, reply, like, DM, upload media, delete, " +
	"or inspect the user's authenticated X account — those require a separate authenticated X API surface outside this tool. Available " +
	"when xAI credentials are configured (SuperGrok OAuth or XAI_API_KEY)."

type registrant struct {
	providers *provider.Registry
	creds     *credentials.Registry
}

func Register(registry *tools.Registry, providers *provider.Registry, creds *credentials.Registry) {
	registry.Register(registrant{providers, creds})
}

func (r registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:    "x_search",
		Toolset: "x_search",
		Title:   "X search",
		About:   "Search posts on X, the site once called Twitter",
		Group:   "X search",
		Icon:    tools.IconSearch,
		Subject: "query",
		Schema: tools.Schema(description, []string{"query"}, map[string]any{
			"query": tools.Property("string", "What to look up on X."),
			"allowed_x_handles": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Optional list of X handles to include exclusively (max 10)."},
			"excluded_x_handles": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Optional list of X handles to exclude (max 10)."},
			"from_date": tools.Property("string", "Optional start date in YYYY-MM-DD format."),
			"to_date":   tools.Property("string", "Optional end date in YYYY-MM-DD format."),
			"enable_image_understanding": map[string]any{"type": "boolean",
				"description": "Whether xAI should analyze images attached to matching X posts.", "default": false},
			"enable_video_understanding": map[string]any{"type": "boolean",
				"description": "Whether xAI should analyze videos attached to matching X posts.", "default": false},
		}),
		Handler:        tools.Bind(r.search),
		Check:          func(env *tools.Env) bool { _, ok := r.bearer(env.Config); return ok },
		Lacks:          func(*tools.Env) string { return "It needs an xAI account that is connected." },
		MaxResultChars: 100000,
	}}
}

// Bearer of a search, the xAI API key first
func (r registrant) bearer(cfg *config.Config) (web.XAI, bool) {
	return web.XAIBearer(r.providers, r.creds, cfg, true)
}

type arguments struct {
	Query    string  `json:"query"`
	Allowed  handles `json:"allowed_x_handles"`
	Excluded handles `json:"excluded_x_handles"`
	From     string  `json:"from_date"`
	To       string  `json:"to_date"`
	Images   bool    `json:"enable_image_understanding"`
	Videos   bool    `json:"enable_video_understanding"`
}

// X handles as the model gives them: a list, or one handle on its own
type handles []web.Text

func (h *handles) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, (*[]web.Text)(h)) == nil {
		return nil
	}
	var one web.Text
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*h = handles{one}
	return nil
}

// Handles without surrounding spaces and leading @, blank ones left out, at most ten
func (h handles) clean(field string) ([]string, error) {
	var out []string
	for _, handle := range h {
		if name := strings.TrimLeft(strings.TrimSpace(handle.String()), "@"); name != "" {
			out = append(out, name)
		}
	}
	if len(out) > maxHandles {
		return nil, fmt.Errorf("%s supports at most %d handles", field, maxHandles)
	}
	return out, nil
}

type request struct {
	Model     string     `json:"model"`
	Input     []message  `json:"input"`
	Tools     []xSearch  `json:"tools"`
	Store     bool       `json:"store"`
	Reasoning *reasoning `json:"reasoning,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

type xSearch struct {
	Type             string   `json:"type"`
	AllowedXHandles  []string `json:"allowed_x_handles,omitempty"`
	ExcludedXHandles []string `json:"excluded_x_handles,omitempty"`
	FromDate         string   `json:"from_date,omitempty"`
	ToDate           string   `json:"to_date,omitempty"`
	Images           bool     `json:"enable_image_understanding,omitempty"`
	Videos           bool     `json:"enable_video_understanding,omitempty"`
}

// Reply of the Responses API. Parts of another shape hold nothing the result reads and are left empty
type reply struct {
	OutputText web.Text `json:"output_text"`
	Output     []struct {
		Type    string `json:"type"`
		Content []struct {
			Type        string   `json:"type"`
			Text        web.Text `json:"text"`
			Annotations []struct {
				Type       string          `json:"type"`
				URL        web.Text        `json:"url"`
				Title      web.Text        `json:"title"`
				StartIndex json.RawMessage `json:"start_index"`
				EndIndex   json.RawMessage `json:"end_index"`
			} `json:"annotations"`
		} `json:"content"`
	} `json:"output"`
	Citations []json.RawMessage `json:"citations"`
}

// url_citation annotation of the answer, its indices as xAI gave them, null when it gave none
type citation struct {
	URL        string          `json:"url"`
	Title      string          `json:"title"`
	StartIndex json.RawMessage `json:"start_index"`
	EndIndex   json.RawMessage `json:"end_index"`
}

type found struct {
	Success          bool              `json:"success"`
	Provider         string            `json:"provider"`
	CredentialSource string            `json:"credential_source"`
	Tool             string            `json:"tool"`
	Model            string            `json:"model"`
	Query            string            `json:"query"`
	Answer           string            `json:"answer"`
	Citations        []json.RawMessage `json:"citations"`
	InlineCitations  []citation        `json:"inline_citations"`
	// No citations came back although filters narrowed the search, so they may have matched nothing
	Degraded       bool    `json:"degraded"`
	DegradedReason *string `json:"degraded_reason"`
}

// Searches X through xAI's x_search tool and answers with what Grok found and the posts it cites
func (r registrant) search(ctx context.Context, call *tools.Call, args arguments) (string, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", errors.New("query is required for x_search")
	}
	cfg := call.Env.Config
	bearer, ok := r.bearer(cfg)
	if !ok {
		return "", errors.New("No xAI credentials available. Connect xAI or xAI Grok OAuth with /providers, or set XAI_API_KEY.")
	}
	tool, filters, err := narrowed(args)
	if err != nil {
		return "", err
	}
	model := cmp.Or(strings.TrimSpace(cfg.GetString(xsearchconfig.Model)), xsearchconfig.DefaultModel)
	body := request{Model: model, Input: []message{{Role: "user", Content: query}}, Tools: []xSearch{tool}}
	if effort := strings.TrimSpace(cfg.GetString(xsearchconfig.ReasoningEffort)); effort != "" {
		body.Reasoning = &reasoning{Effort: effort}
	}
	seconds := max(cfg.GetInt(xsearchconfig.TimeoutSeconds), minTimeout)
	send := func(bearer web.XAI) (*web.Response, error) {
		return post(ctx, web.Request{
			Method:  http.MethodPost,
			URL:     bearer.BaseURL + "/responses",
			Header:  map[string]string{"Authorization": "Bearer " + bearer.Key, "User-Agent": provider.UserAgent("Icebath-Agent")},
			Body:    body,
			Timeout: time.Duration(seconds) * time.Second,
		}, max(cfg.GetInt(xsearchconfig.Retries), 0))
	}
	response, err := send(bearer)
	// A Grok sign in xAI no longer takes is renewed and the search sent once more with the renewed token
	if refused, ok := errors.AsType[*statusError](err); ok && refused.response.Status == http.StatusUnauthorized && bearer.Source == web.XAIOAuth &&
		web.RenewXAI(ctx, r.providers, r.creds, cfg) == nil {
		if renewed, ok := r.bearer(cfg); ok {
			bearer = renewed
			response, err = send(bearer)
		}
	}
	if err != nil {
		return failed(err, seconds), nil
	}
	text := web.Decoded(response.Header.Get("Content-Type"), response.Body)
	var answer reply
	if invalid, ok := errors.AsType[*json.SyntaxError](json.Unmarshal(text, &answer)); ok {
		return failure(invalid.Error(), "JSONDecodeError"), nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(text), []byte("{")) {
		return failure("the X search reply is not a JSON object", "AttributeError"), nil
	}
	result := found{Success: true, Provider: "xai", CredentialSource: bearer.Source, Tool: "x_search", Model: model, Query: query,
		Citations: answer.Citations, InlineCitations: []citation{}}
	if result.Citations == nil {
		result.Citations = []json.RawMessage{}
	}
	var parts []string
	for _, item := range answer.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			if part := strings.TrimSpace(content.Text.String()); part != "" && (content.Type == "output_text" || content.Type == "text") {
				parts = append(parts, part)
			}
			for _, note := range content.Annotations {
				if note.Type == "url_citation" {
					result.InlineCitations = append(result.InlineCitations,
						citation{URL: note.URL.String(), Title: note.Title.String(), StartIndex: note.StartIndex, EndIndex: note.EndIndex})
				}
			}
		}
	}
	result.Answer = cmp.Or(strings.TrimSpace(answer.OutputText.String()), strings.Join(parts, "\n\n"))
	if len(filters) > 0 && len(result.Citations) == 0 && len(result.InlineCitations) == 0 {
		reason := "no citations returned despite filters: " + strings.Join(filters, ", ")
		result.Degraded, result.DegradedReason = true, &reason
	}
	return tools.JSON(result), nil
}

// x_search tool of the request and the names of the filters that narrow it, an error for filters xAI would refuse
func narrowed(args arguments) (xSearch, []string, error) {
	allowed, err := args.Allowed.clean("allowed_x_handles")
	if err != nil {
		return xSearch{}, nil, err
	}
	excluded, err := args.Excluded.clean("excluded_x_handles")
	if err != nil {
		return xSearch{}, nil, err
	}
	if len(allowed) > 0 && len(excluded) > 0 {
		return xSearch{}, nil, errors.New("allowed_x_handles and excluded_x_handles cannot be used together")
	}
	from, to, err := dates(args.From, args.To)
	if err != nil {
		return xSearch{}, nil, err
	}
	var filters []string
	if len(allowed) > 0 {
		filters = append(filters, "allowed_x_handles")
	}
	if len(excluded) > 0 {
		filters = append(filters, "excluded_x_handles")
	}
	if from != "" {
		filters = append(filters, "from_date")
	}
	if to != "" {
		filters = append(filters, "to_date")
	}
	return xSearch{Type: "x_search", AllowedXHandles: allowed, ExcludedXHandles: excluded, FromDate: from, ToDate: to,
		Images: args.Images, Videos: args.Videos}, filters, nil
}

// Dates of the range as xAI reads them, empty when not given: real days, from on or before to, and from not after today in UTC
func dates(from, to string) (string, string, error) {
	start, hasStart, err := day(from, "from_date")
	if err != nil {
		return "", "", err
	}
	end, hasEnd, err := day(to, "to_date")
	if err != nil {
		return "", "", err
	}
	if hasStart && hasEnd && start.After(end) {
		return "", "", fmt.Errorf("from_date (%s) must be on or before to_date (%s)", start.Format(time.DateOnly), end.Format(time.DateOnly))
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if hasStart && start.After(today) {
		return "", "", fmt.Errorf("from_date (%s) is in the future; X Search only indexes past posts (today UTC is %s)",
			start.Format(time.DateOnly), today.Format(time.DateOnly))
	}
	var fromDate, toDate string
	if hasStart {
		fromDate = start.Format(time.DateOnly)
	}
	if hasEnd {
		toDate = end.Format(time.DateOnly)
	}
	return fromDate, toDate, nil
}

// Day a date argument names, month and day with or without a leading zero; false when it is blank
func day(value, field string) (time.Time, bool, error) {
	if value = strings.TrimSpace(value); value == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse("2006-1-2", value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s must be YYYY-MM-DD (got %q)", field, value)
	}
	return parsed, true, nil
}

// Error answer of xAI
type statusError struct {
	response *web.Response
}

// Status line naming the address that answered
func (e *statusError) Error() string {
	class := "Client"
	if e.response.Status >= 500 {
		class = "Server"
	}
	return fmt.Sprintf("%d %s Error: %s for url: %s", e.response.Status, class, http.StatusText(e.response.Status), e.response.URL)
}

// What xAI said: the code and error of its JSON answer, else its answer as text, else the status line
func (e *statusError) message() string {
	text := strings.TrimSpace(e.response.Text())
	var detail map[string]web.Text
	if json.Unmarshal([]byte(text), &detail) != nil || detail == nil {
		if text == "" {
			return e.Error()
		}
		runes := []rune(text)
		return string(runes[:min(len(runes), 500)])
	}
	code := strings.TrimSpace(detail["code"].String())
	message := cmp.Or(strings.TrimSpace(detail["error"].String()), text)
	if code != "" && !strings.Contains(message, code) {
		message = code + ": " + message
	}
	return message
}

// Failure to reach xAI or to read its answer, of a kind error_type names: ReadTimeout, ConnectTimeout or ConnectionError
type requestError struct {
	kind string
	err  error
}

func (e *requestError) Error() string { return e.err.Error() }

func (e *requestError) Unwrap() error { return e.err }

// Kind of a failure to reach xAI
func transport(err error) string {
	if timeout, ok := errors.AsType[net.Error](err); !ok || !timeout.Timeout() {
		return "ConnectionError"
	}
	if dial, ok := errors.AsType[*net.OpError](err); ok && dial.Op == "dial" {
		return "ConnectTimeout"
	}
	return "ReadTimeout"
}

// Answer of xAI. Server errors, timeouts and connection failures are tried again up to retries times, after a pause that grows by a
// second and a half with each attempt up to five seconds
func post(ctx context.Context, request web.Request, retries int) (*web.Response, error) {
	for attempt := 0; ; attempt++ {
		response, err := web.Send(ctx, request)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && attempt >= retries:
			return nil, &requestError{kind: transport(err), err: err}
		case err == nil && response.Status < 400:
			return response, nil
		case err == nil && (response.Status < 500 || attempt >= retries):
			return nil, &statusError{response}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(min(5, 1.5*float64(attempt+1)) * float64(time.Second))):
		}
	}
}

// Failure JSON of a search that got no usable answer
func failed(err error, seconds int) string {
	if refused, ok := errors.AsType[*statusError](err); ok {
		return failure(refused.message(), "HTTPError")
	}
	unreached, ok := errors.AsType[*requestError](err)
	switch {
	case !ok:
		return failure(err.Error(), "RuntimeError")
	case unreached.kind == "ReadTimeout":
		return failure(fmt.Sprintf("xAI x_search timed out after %d seconds", seconds), unreached.kind)
	}
	return failure(err.Error(), unreached.kind)
}

// Failure the model reads, error_type naming the kind of error
func failure(message, kind string) string {
	return tools.JSON(struct {
		Success   bool   `json:"success"`
		Provider  string `json:"provider"`
		Tool      string `json:"tool"`
		Error     string `json:"error"`
		ErrorType string `json:"error_type"`
	}{Provider: "xai", Tool: "x_search", Error: message, ErrorType: kind})
}

// The group of X search, whose options are the X search section
func (r registrant) Groups() []tools.Group {
	return []tools.Group{{Title: "X search", Sections: []string{xsearchconfig.Key}, Order: 35}}
}

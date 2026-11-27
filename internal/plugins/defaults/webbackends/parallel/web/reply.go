package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/web"
)

// Result of a search
type hit struct {
	URL      required  `json:"url"`
	Title    web.Text  `json:"title"`
	Excerpts web.Texts `json:"excerpts"`
}

func (h hit) at(position int) web.Hit {
	return web.Hit{URL: h.URL.String(), Title: h.Title.String(), Description: strings.Join(h.Excerpts, " "), Position: position}
}

// Page read
type document struct {
	URL         required        `json:"url"`
	Title       web.Text        `json:"title"`
	FullContent json.RawMessage `json:"full_content"`
	Content     json.RawMessage `json:"content"`
	Excerpts    web.Texts       `json:"excerpts"`
}

// The page with the first of the contents given that holds something, else its excerpts a paragraph each
func (d document) page(contents ...json.RawMessage) *web.Page {
	text := strings.Join(d.Excerpts, "\n\n")
	for _, raw := range contents {
		var value web.Text
		if web.Holds(raw) && json.Unmarshal(raw, &value) == nil {
			text = value.String()
			break
		}
	}
	return web.Document(d.URL.String(), d.Title.String(), text)
}

// Page Parallel could not read
type refusal struct {
	URL       required `json:"url"`
	Status    any      `json:"http_status_code"`
	ErrorType required `json:"error_type"`
	Content   web.Text `json:"content"`
}

func (r refusal) page() *web.Page {
	return web.Refused(r.URL.String(), r.Status, r.ErrorType.String(), r.Content.String())
}

// Text of a field the SDK's model requires, set once the reply has it
type required struct {
	web.Text
	set bool
}

func (r *required) UnmarshalJSON(data []byte) error {
	r.set = true
	return r.Text.UnmarshalJSON(data)
}

// Failure of a reply that lacks a field its model requires
func missing(name, field string) error {
	return fmt.Errorf("the %s reply has no %s field", name, field)
}

// Decodes a reply object into v, failing with what it got and the field read first from it when it is something else
func model(data []byte, field string, v any) error {
	if got := web.Kind(data); got != web.KindMapping {
		return fmt.Errorf("expected a mapping with %s, got %s", field, got)
	}
	return json.Unmarshal(data, v)
}

// Items of a list, none when the value holds nothing
func items(data []byte) ([]json.RawMessage, error) {
	if !web.Holds(data) {
		return nil, nil
	}
	if got := web.Kind(data); got != web.KindList {
		return nil, fmt.Errorf("expected a list, got %s", got)
	}
	var out []json.RawMessage
	return out, json.Unmarshal(data, &out)
}

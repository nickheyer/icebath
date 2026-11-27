package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/nickheyer/icebath/internal/web"
)

// One result of a search or a page read
type result struct {
	URL        web.Text               `json:"url"`
	Title      web.Text               `json:"title"`
	Text       web.Text               `json:"text"`
	Highlights web.Texts              `json:"highlights"`
	Entities   optional[list[entity]] `json:"entities"`
}

// How reading one page went
type status struct {
	ID     web.Text `json:"id"`
	Status web.Text `json:"status"`
	Error  cause    `json:"error"`
}

// Why Exa could not read a page, given only by an object
type cause struct {
	Tag  web.Text `json:"tag"`
	Code any      `json:"httpStatusCode"`
}

func (c *cause) UnmarshalJSON(data []byte) error {
	if web.Kind(data) != web.KindMapping {
		return nil
	}
	type fields cause
	return json.Unmarshal(data, (*fields)(c))
}

// Origin status of a page Exa could not read: the HTTP status it gives when that is one, else the one its crawl tag names
func (c cause) origin() any {
	if code, ok := web.Status(c.Code); ok {
		return code
	}
	if code, ok := crawlStatus(c.Tag.String()); ok {
		return code
	}
	return nil
}

// Crawl failure tag ending a text, not glued to a word before it: the page's HTTP status in any script, or that it was not found
var crawl = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_])CRAWL_(?:HTTP_(\p{Nd}{3})|NOT_FOUND)[\s\v\p{Z}\x{1c}-\x{1f}\x{85}]*$`)

// Origin status a crawl failure tag names, 404 for a page not found
func crawlStatus(text string) (int, bool) {
	match := crawl.FindStringSubmatch(text)
	switch {
	case match == nil:
		return 0, false
	case match[1] == "":
		return 404, true
	}
	return web.Status(match[1])
}

// Results of an answer, and its statuses when asked for, as exa-py reads them: an object whose cost block can be read, then its
// statuses, then the list of results it must have, each with entities exa-py can parse
func read(body []byte, statuses bool) ([]result, []status, error) {
	var answer struct {
		Results     json.RawMessage            `json:"results"`
		Statuses    json.RawMessage            `json:"statuses"`
		CostDollars optional[record[struct{}]] `json:"costDollars"`
	}
	if err := web.Object(body, &answer); err != nil {
		return nil, nil, err
	}
	var failed list[status]
	if statuses && answer.Statuses != nil {
		if err := json.Unmarshal(answer.Statuses, &failed); err != nil {
			return nil, nil, err
		}
	}
	if answer.Results == nil {
		return nil, nil, errors.New("the reply has no results field")
	}
	var rows list[result]
	if err := json.Unmarshal(answer.Results, &rows); err != nil {
		return nil, nil, err
	}
	return rows, failed, nil
}

// Entity exa-py parses from a result: a company or a person needs an id, a version and properties it can read, any other kind is skipped
type entity struct{}

func (*entity) UnmarshalJSON(data []byte) error {
	var fields struct {
		Type       any             `json:"type"`
		ID         json.RawMessage `json:"id"`
		Version    json.RawMessage `json:"version"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var properties any
	switch fields.Type {
	case "company":
		properties = &company{}
	case "person":
		properties = &person{}
	default:
		return nil
	}
	switch {
	case fields.ID == nil:
		return fmt.Errorf("a %s entity has no id field", fields.Type)
	case fields.Version == nil:
		return fmt.Errorf("a %s entity has no version field", fields.Type)
	case fields.Properties == nil:
		return nil
	}
	return web.Object(fields.Properties, properties)
}

// Nested block of an entity, read when it holds something
type block = optional[record[struct{}]]

type company struct {
	Workforce    block                        `json:"workforce"`
	Headquarters block                        `json:"headquarters"`
	Financials   optional[record[financials]] `json:"financials"`
	WebTraffic   block                        `json:"webTraffic"`
}

type financials struct {
	FundingLatestRound block `json:"fundingLatestRound"`
}

type person struct {
	WorkHistory      *list[job]                 `json:"workHistory"`
	EducationHistory *list[schooling]           `json:"educationHistory"`
	Research         optional[record[research]] `json:"research"`
}

type job struct {
	Dates   block `json:"dates"`
	Company block `json:"company"`
}

type schooling struct {
	Dates       block `json:"dates"`
	Institution block `json:"institution"`
}

type research struct {
	NotableWorks *list[struct{}] `json:"notableWorks"`
}

// JSON object read into T
type record[T any] struct{ value T }

func (r *record[T]) UnmarshalJSON(data []byte) error {
	return web.Object(data, &r.value)
}

// JSON list of objects each read into T
type list[T any] []T

func (l *list[T]) UnmarshalJSON(data []byte) error {
	if got := web.Kind(data); got != web.KindList {
		return fmt.Errorf("expected a list, got %s", got)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	*l = make(list[T], len(items))
	for i, item := range items {
		if err := web.Object(item, &(*l)[i]); err != nil {
			return err
		}
	}
	return nil
}

// JSON value read into T only when it holds something, as exa-py reads such fields
type optional[T any] struct{}

func (*optional[T]) UnmarshalJSON(data []byte) error {
	if !web.Holds(data) {
		return nil
	}
	var value T
	return json.Unmarshal(data, &value)
}

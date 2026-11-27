package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/elevenlabs/config"
	"github.com/nickheyer/icebath/internal/speech"
)

var (
	sources = []string{"library", "account"}
	sorts   = []string{"usage_character_count_1y", "trending", "cloned_by_count", "created_date"}
	filters = []string{"gender", "age", "accent", "language"}
)

const (
	pageSize      = 20
	maxPageSize   = 100
	describeChars = 240
	searchTimeout = 20 * time.Second
)

// Schema of the find_voices argument
func searchSchema() map[string]any {
	text := func(about string) map[string]any { return map[string]any{"type": "string", "description": about} }
	return map[string]any{
		"type": "object",
		"description": "Search voices instead of speaking (text is then not needed). Returns a page of voices with voice_id, labels, description and " +
			"preview_url; pass next_page back as page. source 'library' (default) = the public Voice Library, thousands of community voices, most-used " +
			"first, usable directly by voice_id; 'account' = the user's own voices. The library search matches words in names and descriptions literally: " +
			"when a role word ('dwarf', 'blacksmith') finds nothing, describe the sound instead ('gravelly old', 'deep growl') with gender/age and use_case " +
			"'characters_animation'. You can't hear voices: choose from labels and description, and give the user preview_url links when the pick matters.",
		"properties": map[string]any{
			"query":     text("Free-text search, e.g. 'orc' or 'gravelly old'."),
			"source":    map[string]any{"type": "string", "enum": sources},
			"gender":    text("e.g. male, female, neutral."),
			"age":       text("e.g. young, middle_aged, old."),
			"accent":    text("e.g. british, american, scottish."),
			"language":  text("ISO 639-1 code, e.g. en."),
			"use_case":  text("Library only, e.g. characters_animation, narrative_story, conversational."),
			"sort":      map[string]any{"type": "string", "enum": sorts, "description": "Library only; default usage_character_count_1y (most used)."},
			"page":      text("next_page from the previous result."),
			"page_size": map[string]any{"type": "integer", "description": "1-100, default 20."},
		},
	}
}

// Description with its space collapsed, cut with an ellipsis
func clip(value any) string {
	text := strings.Join(strings.Fields(speech.Text(value)), " ")
	if utf8.RuneCountInString(text) <= describeChars {
		return text
	}
	return string([]rune(text)[:describeChars-1]) + "…"
}

// One compact voice, the traits a library voice has at its top level and an account voice under its labels
func row(voice map[string]any) map[string]any {
	labels, _ := voice["labels"].(map[string]any)
	out := map[string]any{"voice_id": voice["voice_id"], "name": voice["name"]}
	for _, key := range []string{"gender", "age", "accent", "language", "descriptive", "use_case"} {
		value := voice[key]
		if !speech.Held(value) {
			value = labels[key]
		}
		if !speech.Held(value) {
			value = labels[strings.ReplaceAll(key, "_", " ")]
		}
		if speech.Held(value) {
			out[key] = value
		}
	}
	if speech.Held(voice["category"]) {
		out["category"] = voice["category"]
	}
	if speech.Held(voice["description"]) {
		out["description"] = clip(voice["description"])
	}
	if rate, ok := voice["rate"]; ok && rate != nil {
		if f, isNumber := rate.(float64); !isNumber || f != 1 {
			out["credit_rate"] = rate
		}
	}
	if speech.Held(voice["preview_url"]) {
		out["preview_url"] = voice["preview_url"]
	}
	return out
}

// Whole number of a page cursor, 0 for none
func page(text string) int {
	if text == "" {
		return 0
	}
	n, _ := strconv.Atoi(text)
	return n
}

func digits(text string) bool {
	return text != "" && strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// Path and query of one page of a source
func params(source, query string, picked [][2]string, useCase, order, cursor string, size int) (string, url.Values, error) {
	values := url.Values{}
	values.Set("page_size", strconv.Itoa(size))
	for _, f := range picked {
		values.Set(f[0], f[1])
	}
	if query != "" {
		values.Set("search", query)
	}
	if source == "account" {
		values.Set("include_total_count", "false")
		if cursor != "" {
			values.Set("next_page_token", cursor)
		}
		return "/v2/voices", values, nil
	}
	if cursor != "" && !digits(cursor) {
		return "", nil, fmt.Errorf("Invalid library page %s: pass next_page from the previous result.", cursor)
	}
	if order != "" && !slices.Contains(sorts, order) {
		return "", nil, errors.New("find_voices.sort must be one of " + strings.Join(sorts, ", "))
	}
	values.Set("sort", cmpOr(order, sorts[0]))
	if useCase != "" {
		values.Add("use_cases", useCase)
	}
	values.Set("page", strconv.Itoa(page(cursor)))
	return "/v1/shared-voices", values, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Cursor of the next page, nil when the source has no more
func next(source string, body map[string]any, cursor string) any {
	if !speech.Held(body["has_more"]) {
		return nil
	}
	if source == "account" {
		if speech.Held(body["next_page_token"]) {
			return body["next_page_token"]
		}
		return nil
	}
	return strconv.Itoa(page(cursor) + 1)
}

// One page of ElevenLabs voices of the library or the account
func find(ctx context.Context, job *speech.Job, query map[string]any) (map[string]any, error) {
	apiKey := key(job)
	if apiKey == "" {
		return nil, errors.New(config.KeyEnv + " not set. Get one at " + config.Link)
	}
	arg := func(name string) string {
		if !speech.Held(query[name]) {
			return ""
		}
		return strings.TrimSpace(speech.Text(query[name]))
	}
	source := strings.ToLower(cmpOr(arg("source"), sources[0]))
	if !slices.Contains(sources, source) {
		return nil, errors.New("find_voices.source must be one of " + strings.Join(sources, ", "))
	}
	size := pageSize
	if raw := query["page_size"]; speech.Held(raw) {
		if n, err := cast.ToIntE(raw); err == nil {
			size = max(1, min(maxPageSize, n))
		}
	}
	var picked [][2]string
	for _, name := range filters {
		if value := arg(name); value != "" {
			picked = append(picked, [2]string{name, strings.ToLower(value)})
		}
	}
	cursor := arg("page")
	path, values, err := params(source, arg("query"), picked, strings.ToLower(arg("use_case")), strings.ToLower(arg("sort")), cursor, size)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, base(job)+path+"?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("xi-api-key", apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("ElevenLabs voice search failed: %v", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("ElevenLabs voice search failed: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		slog.Warn(fmt.Sprintf("ElevenLabs %s returned HTTP %d", path, response.StatusCode))
		return nil, fmt.Errorf("ElevenLabs voice search failed (HTTP %d): %s", response.StatusCode, speech.Prefix(string(raw), 300))
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("ElevenLabs voice search returned a non-JSON body")
	}
	voices := []any{}
	listed, _ := body["voices"].([]any)
	for _, v := range listed {
		if voice, ok := v.(map[string]any); ok {
			voices = append(voices, row(voice))
		}
	}
	var configured any
	if id := job.Text(config.VoiceID, ""); id != "" {
		configured = id
	}
	result := map[string]any{"success": true, "source": source, "count": len(voices), "voices": voices, "next_page": next(source, body, cursor),
		"configured_voice_id": configured}
	if source == "library" && speech.Held(body["total_count"]) {
		result["total"] = body["total_count"]
	}
	return result, nil
}

package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	elevenLabsVoiceSources = []string{"library", "account"}
	elevenLabsLibrarySorts = []string{"usage_character_count_1y", "trending", "cloned_by_count", "created_date"}
	elevenLabsVoiceFilters = []string{"gender", "age", "accent", "language"}
)

const (
	voicesDefaultPageSize     = 20
	voicesMaxPageSize         = 100
	voicesDescriptionMaxChars = 240
	voicesTimeout             = 20 * time.Second
)

// Parameter schema of the find_voices argument
func findVoicesSchema() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return map[string]any{
		"type": "object",
		"description": "Search voices instead of speaking (text is then not needed). Returns a page of voices " +
			"with voice_id, labels, description and preview_url; pass next_page back as page. " +
			"source 'library' (default) = the public Voice Library, thousands of community voices, " +
			"most-used first, usable directly by voice_id; 'account' = the user's own voices. The " +
			"library search matches words in names and descriptions literally: when a role word " +
			"('dwarf', 'blacksmith') finds nothing, describe the sound instead ('gravelly old', " +
			"'deep growl') with gender/age and use_case 'characters_animation'. You can't hear " +
			"voices: choose from labels and description, and give the user preview_url links when " +
			"the pick matters.",
		"properties": map[string]any{
			"query":    str("Free-text search, e.g. 'orc' or 'gravelly old'."),
			"source":   map[string]any{"type": "string", "enum": []any{"library", "account"}},
			"gender":   str("e.g. male, female, neutral."),
			"age":      str("e.g. young, middle_aged, old."),
			"accent":   str("e.g. british, american, scottish."),
			"language": str("ISO 639-1 code, e.g. en."),
			"use_case": str("Library only, e.g. characters_animation, narrative_story, conversational."),
			"sort": map[string]any{"type": "string", "enum": []any{"usage_character_count_1y", "trending", "cloned_by_count", "created_date"},
				"description": "Library only; default usage_character_count_1y (most used)."},
			"page":      str("next_page from the previous result."),
			"page_size": map[string]any{"type": "integer", "description": "1-100, default 20."},
		},
	}
}

// Whitespace collapsed description clipped with an ellipsis
func voiceClip(text any) string {
	if !pyTruthy(text) {
		text = ""
	}
	s := strings.Join(pystr.SplitWhitespace(pyrepr.Str(text), -1), " ")
	if utf8.RuneCountInString(s) <= voicesDescriptionMaxChars {
		return s
	}
	return string([]rune(s)[:voicesDescriptionMaxChars-1]) + "…"
}

// One compact voice row, library traits at top level and account traits under labels
func voiceRow(voice map[string]any) *pyjson.Object {
	labels, _ := asStrMap(voice["labels"])
	row := pyjson.ObjectOf("voice_id", voice["voice_id"], "name", voice["name"])
	for _, key := range []string{"gender", "age", "accent", "language", "descriptive", "use_case"} {
		value := voice[key]
		if !pyTruthy(value) {
			value = labels[key]
		}
		if !pyTruthy(value) {
			value = labels[strings.ReplaceAll(key, "_", " ")]
		}
		if pyTruthy(value) {
			row.Set(key, value)
		}
	}
	if pyTruthy(voice["category"]) {
		row.Set("category", voice["category"])
	}
	if pyTruthy(voice["description"]) {
		row.Set("description", voiceClip(voice["description"]))
	}
	if rate, ok := voice["rate"]; ok && rate != nil {
		if f, isNum := pyRealNumber(rate); !isNum || f != 1 {
			row.Set("credit_rate", rate)
		}
	}
	if pyTruthy(voice["preview_url"]) {
		row.Set("preview_url", voice["preview_url"])
	}
	return row
}

// Path and ordered query parameters of one voice source, a value error for a bad cursor or sort
func voiceRequestParams(source, query string, filters [][2]string, useCase, sort, page string, pageSize int) (string, url.Values, error) {
	params := url.Values{}
	params.Set("page_size", strconv.Itoa(pageSize))
	for _, f := range filters {
		params.Set(f[0], f[1])
	}
	if query != "" {
		params.Set("search", query)
	}
	if source == "account" {
		params.Set("include_total_count", "false")
		if page != "" {
			params.Set("next_page_token", page)
		}
		return "/v2/voices", params, nil
	}
	if page != "" && !pystr.StrIsDigit(page) {
		return "", nil, &ttsValueError{msg: fmt.Sprintf("Invalid library page %s: pass next_page from the previous result.", pyrepr.Repr(page))}
	}
	if sort != "" && !containsString(elevenLabsLibrarySorts, sort) {
		return "", nil, &ttsValueError{msg: "find_voices.sort must be one of " + strings.Join(elevenLabsLibrarySorts, ", ")}
	}
	if sort == "" {
		sort = elevenLabsLibrarySorts[0]
	}
	params.Set("sort", sort)
	if useCase != "" {
		params.Add("use_cases", useCase)
	}
	n, _ := pyInt(orZeroString(page))
	params.Set("page", strconv.Itoa(n))
	return "/v1/shared-voices", params, nil
}

// The page text or zero for an empty page
func orZeroString(page string) any {
	if page == "" {
		return 0
	}
	return page
}

// Cursor of the next page, nil when the source reports no more
func voiceNextPage(source string, body map[string]any, page string) any {
	if !pyTruthy(body["has_more"]) {
		return nil
	}
	if source == "account" {
		if pyTruthy(body["next_page_token"]) {
			return body["next_page_token"]
		}
		return nil
	}
	n, _ := pyInt(orZeroString(page))
	return strconv.Itoa(n + 1)
}

// Voice search GET with the xi-api-key header, the seam tests replace
var elevenLabsVoicesGet = func(ctx context.Context, rawURL, apiKey string) (int, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, voicesTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// One compact paginated page of ElevenLabs voices from the library or the account
func searchVoices(ctx context.Context, find map[string]any, ttsConfig map[string]any, apiKey string) string {
	arg := func(name string) string { return pystr.Strip(pyrepr.Str(orEmptyString(find[name]))) }
	source := pystr.Lower(arg("source"))
	if arg("source") == "" {
		source = elevenLabsVoiceSources[0]
	}
	if !containsString(elevenLabsVoiceSources, source) {
		return ttsToolError("find_voices.source must be one of " + strings.Join(elevenLabsVoiceSources, ", "))
	}
	size := voicesDefaultPageSize
	raw := find["page_size"]
	if !pyTruthy(raw) {
		raw = voicesDefaultPageSize
	}
	if n, err := pyInt(raw); err == nil {
		size = max(1, min(voicesMaxPageSize, n))
	}
	var filters [][2]string
	for _, name := range elevenLabsVoiceFilters {
		if v := arg(name); v != "" {
			filters = append(filters, [2]string{name, pystr.Lower(v)})
		}
	}
	page := arg("page")
	path, params, err := voiceRequestParams(source, arg("query"), filters, pystr.Lower(arg("use_case")), pystr.Lower(arg("sort")), page, size)
	if err != nil {
		return ttsToolError(err.Error())
	}
	elConfig, err := orEmptyDict(ttsConfig, "elevenlabs")
	if err != nil {
		return ttsToolError(err.Error())
	}
	base := defaultElevenLabsBaseURL
	if pyTruthy(elConfig["base_url"]) {
		base = pyrepr.Str(elConfig["base_url"])
	}
	base = strings.TrimRight(base, "/")
	status, raw2, err := elevenLabsVoicesGet(ctx, base+path+"?"+params.Encode(), apiKey)
	if err != nil {
		return ttsToolError(fmt.Sprintf("ElevenLabs voice search failed: %v", err))
	}
	if status != 200 {
		ttsVoicesLog().Warn(fmt.Sprintf("ElevenLabs %s returned HTTP %d", path, status))
		return ttsToolError(fmt.Sprintf("ElevenLabs voice search failed (HTTP %d): %s", status, clipRunes(strings.ToValidUTF8(string(raw2), "�"), 300)))
	}
	var decoded any
	if err := json.Unmarshal(raw2, &decoded); err != nil {
		return ttsToolError("ElevenLabs voice search returned a non-JSON body")
	}
	body, _ := decoded.(map[string]any)
	voices := []any{}
	list, _ := body["voices"].([]any)
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			voices = append(voices, voiceRow(m))
		}
	}
	configured := elConfig["voice_id"]
	if !pyTruthy(configured) {
		configured = nil
	}
	result := pyjson.ObjectOf("success", true, "source", source, "count", len(voices), "voices", voices,
		"next_page", voiceNextPage(source, body, page), "configured_voice_id", configured)
	if source == "library" && pyTruthy(body["total_count"]) {
		result.Set("total", body["total_count"])
	}
	return ttsDumps(result)
}

// Logger of the voice search module
func ttsVoicesLog() *slog.Logger { return slog.Default().With("logger", "tools.tts_elevenlabs_voices") }

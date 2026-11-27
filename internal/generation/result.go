package generation

import (
	"maps"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Shapes of picture image_generate takes, landscape when the call names another
var Aspects = []string{"landscape", "square", "portrait"}

const DefaultAspect = "landscape"

// Aspect ratios and resolutions video_generate takes when the service names none of its own, and its defaults
var (
	VideoAspects     = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "3:2", "2:3", "21:9"}
	VideoResolutions = []string{"480p", "540p", "720p", "768p", "1080p"}
)

const (
	DefaultVideoAspect     = "16:9"
	DefaultVideoResolution = "720p"
)

// Whole number of an argument: a number cut to its whole part, or a text of digits; nil for anything else, a true or false included
func Whole(value any) *int {
	var n int
	switch v := value.(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil
		}
		n = parsed
	default:
		return nil
	}
	return &n
}

// Shape of picture the text names, lower cased, landscape for anything else
func Aspect(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	for _, aspect := range Aspects {
		if text == aspect {
			return aspect
		}
	}
	return DefaultAspect
}

// Sizes of the OpenAI images API for each shape
var openAISizes = map[string]string{"landscape": "1536x1024", "square": "1024x1024", "portrait": "1024x1536"}

// Size the OpenAI images API and those that copy it take for the shape, square for an unknown one
func Size(aspect string) string {
	if size, ok := openAISizes[aspect]; ok {
		return size
	}
	return openAISizes["square"]
}

// What a call made, or why it failed, as the model reads it
type Result struct {
	OK bool
	// File the picture or video was saved to, else the address it is fetched from
	Media string
	// Copy kept in the cache of what Media is the address of, such as a picture stored at the service for a link to chain into its tools
	Copy     string
	Model    string
	Prompt   string
	Aspect   string
	Provider string
	// What it was made from: text, image, or reference for videos made from references
	Modality string
	// Seconds of a video
	Duration int
	// Why it failed and what kind of failure it was, such as auth_required
	Error string
	Type  string
	// Fields a service adds, such as the revised prompt or the storage link
	Extra map[string]any
}

// Fields every result of one call carries
type Call struct {
	Provider, Model, Prompt, Aspect string
}

// Failure of the call
func (c Call) Fail(text, kind string) Result {
	if kind == "" {
		kind = "provider_error"
	}
	return Result{Error: text, Type: kind, Provider: c.Provider, Model: c.Model, Prompt: c.Prompt, Aspect: c.Aspect}
}

// What the call made, modality text when empty
func (c Call) Made(media, modality string, extra map[string]any) Result {
	if modality == "" {
		modality = "text"
	}
	return Result{OK: true, Media: media, Provider: c.Provider, Model: c.Model, Prompt: c.Prompt, Aspect: c.Aspect, Modality: modality, Extra: extra}
}

// The result as the model reads it: the picture under image or the video under video, the fields of the service after those of the call
func (r Result) Envelope(kind Kind) map[string]any {
	out := maps.Clone(r.Extra)
	if out == nil {
		out = map[string]any{}
	}
	field := string(kind)
	out["success"], out["model"], out["prompt"], out["provider"], out["aspect_ratio"] = r.OK, r.Model, r.Prompt, r.Provider, r.Aspect
	if kind == Image && r.Aspect == "" {
		out["aspect_ratio"] = DefaultAspect
	}
	if !r.OK {
		out[field], out["error"], out["error_type"] = nil, r.Error, r.Type
		return out
	}
	out[field], out["modality"] = r.Media, r.Modality
	if r.Copy != "" {
		out["cached_"+field] = r.Copy
	}
	if kind == Video {
		out["duration"] = r.Duration
	}
	return out
}

// Paths where each count of a usage report may be found, the first one there winning
var (
	promptCounts     = [][]string{{"prompt_tokens"}, {"input_tokens"}}
	outputCounts     = [][]string{{"completion_tokens"}, {"output_tokens"}}
	cacheReadCounts  = [][]string{{"prompt_tokens_details", "cached_tokens"}, {"input_tokens_details", "cached_tokens"}, {"cache_read_input_tokens"}}
	cacheWriteCounts = [][]string{{"prompt_tokens_details", "cache_write_tokens"}, {"cache_creation_input_tokens"}}
	reasoningCounts  = [][]string{{"completion_tokens_details", "reasoning_tokens"}, {"output_tokens_details", "reasoning_tokens"}}
)

// Tokens a usage report of an images or chat API gives, the input counted without its cached part; false without any
func Usage(report any) (provider.Usage, bool) {
	fields, _ := report.(map[string]any)
	if len(fields) == 0 {
		return provider.Usage{}, false
	}
	count := func(paths [][]string) int64 {
		for _, path := range paths {
			var value any = fields
			for _, key := range path {
				object, _ := value.(map[string]any)
				value = object[key]
			}
			if n, ok := value.(float64); ok && n != 0 {
				return int64(n)
			}
		}
		return 0
	}
	u := provider.Usage{Output: count(outputCounts), CacheRead: count(cacheReadCounts), CacheWrite: count(cacheWriteCounts), Reasoning: count(reasoningCounts)}
	u.Input = max(count(promptCounts)-u.CacheRead-u.CacheWrite, 0)
	return u, u != provider.Usage{}
}

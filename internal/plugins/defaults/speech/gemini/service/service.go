// Google Gemini's voices, directed by a persona and, on Gemini 3.1, by audio tags a helper model adds
package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/gemini/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

// PCM Gemini speaks in
const rate = 24000

func Register(registry *speech.Registry) {
	ready := func(job *speech.Job) bool { return key(job) != "" }
	registry.Register(speech.Services{Speakers: []speech.Voice{{
		Name: config.Name, Title: "Google Gemini TTS", About: "30 voices you can direct, needs a Gemini API key",
		Ready:      ready,
		Limit:      func(*speech.Job) int { return config.DefaultLimit },
		Synthesize: synthesize,
		Stream:     &speech.Stream{Rank: 2, Label: "Gemini streaming TTS", Ready: ready, Open: stream},
		Opus:       speech.NativeOpus,
		Setup:      speech.Setup{Keys: []string{config.KeyEnv, config.GoogleKeyEnv}, SignIn: config.Name, Link: config.Link, Badge: "preview"},
	}}})
}

// Gemini key, the Google key standing in when it is unset
func key(job *speech.Job) string {
	return strs.Filled(job.Key(config.KeyEnv, config.Name), job.Key(config.GoogleKeyEnv, config.Name))
}

// Native base of the endpoint: the setting, else GEMINI_BASE_URL, else Google's
func base(job *speech.Job) string {
	return provider.GeminiBase(strs.Filled(job.Text(config.BaseURL, ""), job.Getenv(config.BaseURLEnv)))
}

// Text of the persona file, a relative path under the profile's folder; empty with a warning when it can't be read
func persona(job *speech.Job) string {
	raw := job.Text(config.PersonaFile, "")
	if raw == "" {
		return ""
	}
	path := terminal.ExpandUser(os.ExpandEnv(raw))
	if !filepath.IsAbs(path) {
		path = filepath.Join(job.Config.ProfileDir(job.Config.Profile()), path)
	}
	data, err := os.ReadFile(path)
	if err == nil && utf8.Valid(data) {
		return strings.TrimSpace(strings.TrimPrefix(string(data), string(rune(0xFEFF))))
	}
	if err == nil {
		err = fmt.Errorf("the file is not UTF-8 text")
	}
	slog.Warn(fmt.Sprintf("Gemini TTS persona prompt file unavailable at %s: %v", path, err))
	return ""
}

// Whether audio tags are asked for and the model is a Gemini 3.1 speech model, which knows them
func tags(job *speech.Job, model string) bool {
	if !job.Config.GetBool(config.AudioTags) {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	normalized = normalized[strings.LastIndex(normalized, "/")+1:]
	if strings.Contains(normalized, "gemini-3.1") && strings.Contains(normalized, "tts") {
		return true
	}
	slog.Warn(fmt.Sprintf("Gemini TTS audio_tags enabled, but model %s is not known to support Gemini audio tags; skipping hidden tag rewrite", model))
	return false
}

// Text with audio tags the speech tags model inserted, unchanged when it fails
func tagged(ctx context.Context, job *speech.Job, text, direction string) string {
	transcript := strings.TrimSpace(text)
	if transcript == "" {
		return text
	}
	system := "You rewrite transcripts for Gemini 3.1 Flash TTS by inserting expressive audio tags.\n\n" +
		"Audio tags are inline square-bracket modifiers such as [whispers], [excitedly], [very slow], [sarcastically], [laughs], [sighs], or [gasp]. " +
		"There is no fixed allowlist. Use creative freeform tags generously but naturally to control tone, pace, emotional vibe, emphasis, section-level " +
		"delivery, and non-verbal sounds. Use English audio tags even when the spoken transcript is not English.\n\n" +
		speech.TagRules("- Use square brackets for every audio tag.\n- Do not use SSML or XML tags.\n")
	prompt := "PERSONA AND DIRECTOR CONTEXT:\n" + strs.Filled(strings.TrimSpace(direction), "(none)") + "\n\nTRANSCRIPT TO TAG:\n" + transcript
	return job.Tagged(ctx, system, prompt, text, "Gemini TTS", "untagged text", slog.LevelWarn)
}

// Where the persona file marks the transcript
var slots = []*regexp.Regexp{regexp.MustCompile(`(?i)\{\{\s*transcript\s*\}\}`), regexp.MustCompile(`(?i)\{\s*transcript\s*\}`)}

// Prompt of the persona's direction and the transcript, which fills its slot in the persona when it has one
func compose(text, direction string) string {
	transcript := strings.TrimSpace(text)
	if direction == "" {
		return transcript
	}
	preamble := "Synthesize speech from the TRANSCRIPT only. Treat AUDIO PROFILE, SCENE, DIRECTOR'S NOTES, and SAMPLE CONTEXT as performance direction; do not " +
		"speak those sections aloud."
	for _, slot := range slots {
		if slot.MatchString(direction) {
			return strings.TrimSpace(preamble + "\n\n" + slot.ReplaceAllLiteralString(direction, transcript))
		}
	}
	return strings.TrimSpace(preamble + "\n\n" + direction + "\n\n#### TRANSCRIPT\n" + transcript)
}

// Body of a request for speech of the text in the voice
func body(text, voice string) map[string]any {
	return map[string]any{
		"contents": []any{map[string]any{"parts": []any{map[string]any{"text": text}}}},
		"generationConfig": map[string]any{"responseModalities": []any{"AUDIO"},
			"speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": voice}}}},
	}
}

// Headers of a request: the key, and the client Google's own API is told
func headers(apiKey, address string) map[string]string {
	out := map[string]string{"x-goog-api-key": apiKey}
	if parsed, err := url.Parse(address); err == nil && parsed.Hostname() == "generativelanguage.googleapis.com" {
		out["X-Goog-Api-Client"] = "icebath-agent/" + provider.Version()
	}
	return out
}

// Speech through generateContent: base64 24 kHz mono PCM wrapped as WAV, then the container the path asks for
func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	apiKey := key(job)
	if apiKey == "" {
		return "", speech.Misconfigured(config.KeyEnv + " not set. Get one at " + config.Link)
	}
	model, voice := job.Text(config.Model, config.DefaultModel), job.Text(config.Voice, config.DefaultVoice)
	address := base(job)
	direction := persona(job)
	script := request.Text
	if tags(job, model) {
		script = tagged(ctx, job, script, direction)
	}
	prompt := compose(script, direction)
	voiceOf, _ := job.Speech.Voice(job.Config, config.Name)
	if n, limit := utf8.RuneCountInString(prompt), job.Limit(voiceOf); n > limit {
		return "", speech.Misconfigured(fmt.Sprintf("Gemini TTS composed prompt exceeds the provider request limit (%d > %d chars). Reduce the persona/audio-tag "+
			"prompt or lower tts.gemini.max_text_length so long-form text is split with enough prompt headroom.", n, limit))
	}
	response, err := speech.PostJSON(ctx, address+"/models/"+model+":generateContent", body(prompt, voice), headers(apiKey, address))
	if err != nil {
		return "", err
	}
	if response.StatusCode != 200 {
		raw, err := speech.Body(response, "Gemini TTS", 0)
		if err != nil {
			return "", err
		}
		detail := speech.Prefix(string(raw), 300)
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			if failure, ok := parsed["error"].(map[string]any); ok {
				if message := speech.Field(failure, "message"); message != "" {
					detail = message
				}
			}
		}
		return "", fmt.Errorf("Gemini TTS API error (HTTP %d): %s", response.StatusCode, detail)
	}
	data, err := speech.Object(response, "Gemini TTS")
	if err != nil {
		return "", err
	}
	encoded, err := audio(data)
	if err != nil {
		return "", err
	}
	if encoded == "" {
		return "", fmt.Errorf("Gemini TTS returned empty audio data")
	}
	pcm, err := speech.Base64Std(encoded)
	if err != nil {
		return "", err
	}
	return job.WriteWAV(ctx, speech.WAV(pcm, rate, 1, 2), request.Path)
}

// Base64 audio of the first inline part of the first candidate, a reply without one failing with what it lacks
func audio(data map[string]any) (string, error) {
	malformed := func(detail string) error { return fmt.Errorf("Gemini TTS response was malformed: %s", detail) }
	raw, present := data["candidates"]
	candidates, ok := raw.([]any)
	switch {
	case !present:
		return "", malformed("no candidates")
	case !ok:
		return "", malformed(fmt.Sprintf("candidates is %s where a list was expected", speech.Kind(raw)))
	case len(candidates) == 0:
		return "", malformed("the candidates list is empty")
	}
	first, _ := candidates[0].(map[string]any)
	content, ok := first["content"].(map[string]any)
	if !ok {
		return "", malformed("the first candidate has no content")
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return "", malformed("the first candidate content has no parts")
	}
	for _, p := range parts {
		part, _ := p.(map[string]any)
		_, camel := part["inlineData"]
		_, snake := part["inline_data"]
		if !camel && !snake {
			continue
		}
		inline, _ := part["inlineData"].(map[string]any)
		if len(inline) == 0 {
			inline, _ = part["inline_data"].(map[string]any)
		}
		encoded, _ := inline["data"].(string)
		return encoded, nil
	}
	return "", fmt.Errorf("Gemini TTS response contained no audio data")
}

// PCM of a sentence from streamGenerateContent's server sent events, each inline part decoded as it arrives; the request is made as the
// audio is first read
func stream(ctx context.Context, job *speech.Job, text string) (*speech.PCM, error) {
	return &speech.PCM{Rate: rate, Chunks: func(yield func([]byte, error) bool) {
		address := base(job) + "/models/" + job.Text(config.Model, config.DefaultModel) + ":streamGenerateContent"
		separator := "?"
		if strings.Contains(address, "?") {
			separator = "&"
		}
		response, err := speech.PostJSON(ctx, address+separator+"alt=sse", body(text, job.Text(config.Voice, config.DefaultVoice)), map[string]string{
			"x-goog-api-key": key(job)})
		if err != nil {
			yield(nil, err)
			return
		}
		defer response.Body.Close()
		if err := speech.Failed(response); err != nil {
			yield(nil, err)
			return
		}
		for line, err := range lines(response) {
			if err != nil {
				yield(nil, err)
				return
			}
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var event any
			if json.Unmarshal([]byte(data), &event) != nil {
				continue
			}
			found, ok := eventParts(event)
			if !ok {
				continue
			}
			parts, err := elements(found)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, part := range parts {
				pcm, err := partAudio(part)
				if err != nil {
					yield(nil, err)
					return
				}
				if pcm == nil {
					continue
				}
				if !yield(pcm, nil) {
					return
				}
			}
		}
	}}, nil
}

// The candidates[0].content.parts value of an event, false when the event holds none there
func eventParts(event any) (any, bool) {
	object, _ := event.(map[string]any)
	candidates, _ := object["candidates"].([]any)
	if len(candidates) == 0 {
		return nil, false
	}
	candidate, _ := candidates[0].(map[string]any)
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		return nil, false
	}
	parts, ok := content["parts"]
	return parts, ok
}

// What walking over a JSON value gives: a list's items, a text's characters, an object's keys; refused for any other value
func elements(value any) ([]any, error) {
	switch v := value.(type) {
	case []any:
		return v, nil
	case string:
		out := make([]any, 0, len(v))
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("TTS stream value is %s where a list was expected", speech.Kind(value))
}

// PCM of the inline audio of one part, nil for a part with none or with audio that is not base64, which is warned of
func partAudio(part any) ([]byte, error) {
	object, ok := part.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("TTS stream event is %s where a mapping holding inlineData was expected", speech.Kind(part))
	}
	inline := object["inlineData"]
	if !speech.Held(inline) {
		inline = object["inline_data"]
	}
	if !speech.Held(inline) {
		return nil, nil
	}
	data, ok := inline.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Gemini TTS inline data is %s where a mapping was expected", speech.Kind(inline))
	}
	encoded := data["data"]
	if !speech.Held(encoded) {
		return nil, nil
	}
	text, ok := encoded.(string)
	if !ok {
		slog.Warn(fmt.Sprintf("Gemini SSE: bad base64 audio: audio payload is %s where base64 text was expected", speech.Kind(encoded)))
		return nil, nil
	}
	pcm, err := speech.Base64(text)
	if err != nil {
		slog.Warn(fmt.Sprintf("Gemini SSE: bad base64 audio: %s", err))
		return nil, nil
	}
	return pcm, nil
}

// Text encoding a reply's Content-Type gives its body: its charset, else Latin-1 for text and UTF-8 for JSON; empty when it gives none
func encodingOf(header http.Header) (string, error) {
	kind := strings.Join(header.Values("Content-Type"), ", ")
	if kind == "" {
		return "", nil
	}
	tokens := strings.Split(kind, ";")
	media := strings.TrimSpace(tokens[0])
	charset, set, bare := "", false, false
	for _, param := range tokens[1:] {
		if param = strings.TrimSpace(param); param == "" {
			continue
		}
		name, value, valued := strings.Cut(param, "=")
		if valued {
			name = strings.Trim(name, "\"' ")
		}
		if strings.ToLower(name) == "charset" {
			charset, set, bare = strings.Trim(value, "\"' "), true, !valued
		}
	}
	switch {
	case bare:
		return "", errors.New("Content-Type charset parameter has no value")
	case set:
		return strings.Trim(charset, "'\""), nil
	case strings.Contains(media, "text"):
		return "ISO-8859-1", nil
	case strings.Contains(media, "application/json"):
		return "utf-8", nil
	}
	return "", nil
}

// Decoder of a named text encoding, bytes it can't read turned into U+FFFD
func decoder(name string) (*encoding.Decoder, error) {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "-")) {
	case "utf-8", "utf8":
		return unicode.UTF8.NewDecoder(), nil
	case "iso-8859-1", "latin-1", "latin1":
		return charmap.ISO8859_1.NewDecoder(), nil
	}
	found, err := ianaindex.IANA.Encoding(name)
	if err != nil || found == nil {
		return nil, fmt.Errorf("unknown character set %q", name)
	}
	return found.NewDecoder(), nil
}

// Lines of a streamed reply decoded in the encoding its headers give, split at every line boundary of text; a reply that names no encoding
// fails once it holds a line, which can't be read as text
func lines(response *http.Response) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		name, err := encodingOf(response.Header)
		if err != nil {
			yield("", err)
			return
		}
		if name == "" {
			unreadable := errors.New("the response names no text encoding, so its lines cannot be read as text")
			undecoded := false
			for chunk, err := range speech.Chunks(response.Body, 512, false) {
				if err != nil {
					yield("", err)
					return
				}
				for _, b := range chunk {
					if b != '\n' && b != '\r' {
						undecoded = true
					} else if undecoded {
						yield("", unreadable)
						return
					}
				}
			}
			if undecoded {
				yield("", unreadable)
			}
			return
		}
		decode, err := decoder(name)
		if err != nil {
			yield("", err)
			return
		}
		reader := bufio.NewReader(transform.NewReader(response.Body, decode))
		var line strings.Builder
		for {
			r, _, err := reader.ReadRune()
			if err == io.EOF {
				if line.Len() > 0 {
					yield(line.String(), nil)
				}
				return
			}
			if err != nil {
				yield("", err)
				return
			}
			switch r {
			case '\r':
				if next, _, err := reader.ReadRune(); err == nil && next != '\n' {
					reader.UnreadRune()
				}
				fallthrough
			case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
				if !yield(line.String(), nil) {
					return
				}
				line.Reset()
			default:
				line.WriteRune(r)
			}
		}
	}
}

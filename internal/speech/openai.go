package speech

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/httpx"
)

// Address of OpenAI's API, where an audio client goes when given none
const OpenAIBase = "https://api.openai.com/v1"

// Audio endpoints of OpenAI and the services that copy them, such as Groq and DeepInfra, tried again as OpenAI's SDK tries
type OpenAI struct {
	Key     string
	BaseURL string
	// Tries after the first, and the limit of each try
	Retries int
	Timeout time.Duration
}

// Speech client as OpenAI's SDK makes one: two tries after the first, ten minutes each
func SpeechClient(key, base string) OpenAI {
	return OpenAI{Key: key, BaseURL: base, Retries: 2, Timeout: 600 * time.Second}
}

// Failed status of an OpenAI style API, worded as its SDK words it
type APIError struct {
	Status int
	Body   any
}

func (e *APIError) Error() string { return fmt.Sprintf("Error code: %d - %s", e.Status, Text(e.Body)) }

// A connection to the API that broke, or a try that ran out of time
type ConnectionError struct {
	Timeout bool
	Err     error
}

func (e *ConnectionError) Error() string {
	if e.Timeout {
		return "Request timed out."
	}
	return "Connection error."
}

func (e *ConnectionError) Unwrap() error { return e.Err }

// One OpenAI style speech call
type SpeechRequest struct {
	Model, Voice, Text string
	// File written, whose extension names the format asked for
	Path string
	// 1 for none sent
	Speed        float64
	Instructions string
	// Fields of the body beside OpenAI's own, such as lang_code
	Extra map[string]any
}

// Format of an OpenAI style speech reply the path's extension asks for: opus for .ogg, wav, flac, else mp3
func ResponseFormat(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ogg":
		return "opus"
	case ".wav":
		return "wav"
	case ".flac":
		return "flac"
	}
	return "mp3"
}

// Writes the speech of the request to its path
func (c OpenAI) Speak(ctx context.Context, r SpeechRequest) error {
	body := map[string]any{"model": r.Model, "voice": r.Voice, "input": r.Text, "response_format": ResponseFormat(r.Path)}
	if r.Speed != 1 {
		body["speed"] = max(0.25, min(4, r.Speed))
	}
	if r.Instructions != "" {
		body["instructions"] = r.Instructions
	}
	for name, value := range r.Extra {
		body[name] = value
	}
	return c.Speech(ctx, body, map[string]string{"X-Idempotency-Key": uuid.NewString()}, r.Path)
}

// Writes the speech of a body POSTed to audio/speech to the path
func (c OpenAI) Speech(ctx context.Context, body map[string]any, headers map[string]string, path string) error {
	response, err := c.SpeechStream(ctx, body, headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, response.Body)
	return errors.Join(err, f.Close())
}

// Open reply of a body POSTed to audio/speech, for its audio to be read as it arrives
func (c OpenAI) SpeechStream(ctx context.Context, body map[string]any, headers map[string]string) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, "audio/speech", "application/json", "application/octet-stream", payload, headers)
}

// Transcription of a file POSTed to audio/transcriptions with the fields: the text for response_format text, else the reply's JSON
func (c OpenAI) Transcription(ctx context.Context, path string, fields [][2]string) (any, error) {
	parts := make([]Part, 0, len(fields)+1)
	text := false
	for _, f := range fields {
		parts = append(parts, Part{Name: f[0], Value: f[1]})
		text = text || f[0] == "response_format" && f[1] == "text"
	}
	parts = append(parts, Part{Name: "file", File: path, Type: MediaType(path)})
	form, kind, err := Form(parts)
	if err != nil {
		return nil, err
	}
	response, err := c.send(ctx, "audio/transcriptions", kind, "application/json", form, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, &ConnectionError{Err: err}
	}
	if text {
		return string(raw), nil
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return string(raw), nil
	}
	return decoded, nil
}

// One request tried again on a broken connection and on the failures the SDK tries again, the open reply of the first success
func (c OpenAI) send(ctx context.Context, path, kind, accept string, payload []byte, headers map[string]string) (*http.Response, error) {
	endpoint := strings.TrimRight(cmp.Or(strings.TrimSpace(c.BaseURL), OpenAIBase), "/") + "/" + path
	timeout := cmp.Or(c.Timeout, 600*time.Second)
	for attempt := 0; ; attempt++ {
		bounded, cancel := context.WithTimeout(ctx, timeout)
		request, err := http.NewRequestWithContext(bounded, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			cancel()
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+c.Key)
		request.Header.Set("Content-Type", kind)
		request.Header.Set("Accept", accept)
		request.Header.Set("User-Agent", Agent)
		request.Header.Set("X-Stainless-Retry-Count", strconv.Itoa(attempt))
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			if attempt < c.Retries && ctx.Err() == nil {
				time.Sleep(httpx.Backoff(attempt, nil))
				continue
			}
			ne, isNet := errors.AsType[net.Error](err)
			return nil, &ConnectionError{Timeout: isNet && ne.Timeout() || errors.Is(err, context.DeadlineExceeded), Err: err}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			response.Body = released{ReadCloser: response.Body, cancel: cancel}
			return response, nil
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if attempt < c.Retries && httpx.Retried(response.StatusCode, response.Header) {
			time.Sleep(httpx.Backoff(attempt, response.Header))
			continue
		}
		var detail any = string(raw)
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			if object, ok := parsed.(map[string]any); ok {
				if inner, ok := object["error"]; ok {
					parsed = inner
				}
			}
			detail = parsed
		}
		return nil, &APIError{Status: response.StatusCode, Body: detail}
	}
}

// Body that ends its request's time limit once closed
type released struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r released) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

// Reply of a structured transcription without text, its message as the service gave it
type ReplyError struct{ Message string }

func (e *ReplyError) Error() string { return e.Message }

// Text some services wrap a transcript in, such as Qwen's ASR
var asrText = regexp2.MustCompile(`^\s*language\s+[\w.-]+(?:\s*<audio_language>[^<]*</audio_language>)?\s*<asr_text>\s*(?<text>.*)`,
	regexp2.IgnoreCase|regexp2.Singleline|regexp2.Unicode)

// Plain transcript of a reply that is text, an object with text, or another value; an object without text fails with its error
func TranscriptOf(reply any) (string, error) {
	var text string
	switch v := reply.(type) {
	case string:
		text = strings.TrimSpace(v)
	case map[string]any:
		s, ok := v["text"].(string)
		if !ok {
			if failure := v["error"]; failure != nil && failure != "" && failure != false {
				return "", &ReplyError{Message: Text(failure)}
			}
			return "", &ReplyError{Message: "Transcription response contained no text"}
		}
		text = strings.TrimSpace(s)
	default:
		text = strings.TrimSpace(Text(v))
	}
	if m, _ := asrText.FindStringMatch(text); m != nil {
		return strings.TrimSpace(m.GroupByName("text").String()), nil
	}
	return text, nil
}

// Transcribes through an OpenAI style transcriptions API with the language and prompt hinted; a file the API refuses as unsupported,
// corrupted or invalid, or that a failing service could not take, is sent once more transcoded to compact m4a
func (j *Job) OpenAITranscribe(ctx context.Context, r Recognizer, client OpenAI, path, model string, hints Hints, label string) Transcript {
	create := func(file string) (any, error) {
		format := "json"
		if model == "whisper-1" {
			format = "text"
		}
		fields := [][2]string{{"model", model}, {"response_format", format}}
		if hints.Language != "" {
			name := "language"
			if model == "gpt-transcribe" {
				name = "languages[]"
			}
			fields = append(fields, [2]string{name, hints.Language})
			slog.Debug(fmt.Sprintf("Using language hint '%s' for OpenAI STT", hints.Language))
		}
		if hints.Prompt != "" {
			fields = append(fields, [2]string{"prompt", hints.Prompt})
		}
		return client.Transcription(ctx, file, fields)
	}
	reply, err := create(path)
	if refused, ok := errors.AsType[*APIError](err); ok {
		failing := refused.Status >= 500
		lower := strings.ToLower(refused.Error())
		if failing || strings.Contains(lower, "unsupported") || strings.Contains(lower, "corrupted") || strings.Contains(lower, "invalid file") {
			work, werr := os.MkdirTemp("", "icebath-stt-")
			if werr != nil {
				return r.OpenAIFailure(werr, path, label)
			}
			defer os.RemoveAll(work)
			converted, why := j.Compact(ctx, path, work)
			switch {
			case why != "" && !failing:
				return r.Failure(why)
			case why == "":
				slog.Info(fmt.Sprintf("Retrying %s STT after transcoding %s to m4a (API rejected the original container)", label, filepath.Base(path)))
				reply, err = create(converted)
			}
		}
	}
	if err != nil {
		return r.OpenAIFailure(err, path, label)
	}
	text, err := TranscriptOf(reply)
	if err != nil {
		return r.OpenAIFailure(err, path, label)
	}
	Transcribed(path, label, model, text)
	return Transcript{Success: true, Text: text, Provider: label}
}

// Transcript of a failed call to an OpenAI style transcription API: the file's permission, the reply's own message, the connection, the
// status, else the error itself
func (r Recognizer) OpenAIFailure(err error, path, label string) Transcript {
	if errors.Is(err, os.ErrPermission) {
		return r.Failure("Permission denied: " + path)
	}
	if reply, ok := errors.AsType[*ReplyError](err); ok {
		return r.Failure(reply.Message)
	}
	if _, ok := errors.AsType[*ConnectionError](err); ok {
		return r.Failure("Connection error: " + err.Error())
	}
	if _, ok := errors.AsType[*APIError](err); ok {
		return r.Failure("API error: " + err.Error())
	}
	slog.Error(fmt.Sprintf("%s transcription failed: %v", label, err))
	return r.Failure(fmt.Sprintf("Transcription failed: %v", err))
}

// Transcript of a failed call to a service's own API: the file's permission, the reply's own message, else the label and detail
func (r Recognizer) CloudFailure(err error, path, label, detail string) Transcript {
	if errors.Is(err, os.ErrPermission) {
		return r.Failure("Permission denied: " + path)
	}
	if reply, ok := errors.AsType[*ReplyError](err); ok {
		return r.Failure(reply.Message)
	}
	slog.Error(fmt.Sprintf("%s failed: %v", label, err))
	return r.Failure(label + " failed: " + cmp.Or(detail, err.Error()))
}

// Transcript of a service's reply to an upload: an error status with the detail the reply gives else its start, an empty transcript as
// nothing said, else the text found in the reply
func (r Recognizer) Replied(response *http.Response, err error, path, label string, detail func(map[string]any) string,
	text func(map[string]any) (string, error)) (Transcript, map[string]any) {
	if err != nil {
		return r.CloudFailure(err, path, label+" transcription", ""), nil
	}
	raw, err := Body(response, label, 0)
	if err != nil {
		return r.CloudFailure(err, path, label+" transcription", ""), nil
	}
	if response.StatusCode != http.StatusOK {
		message := Prefix(string(raw), 300)
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			if d := detail(parsed); d != "" {
				message = d
			}
		}
		return r.Failure(fmt.Sprintf("%s API error (HTTP %d): %s", label, response.StatusCode, message)), nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return r.CloudFailure(fmt.Errorf("%s transcription reply is not a JSON object: %w", label, err), path, label+" transcription", ""), nil
	}
	transcript, err := text(body)
	if err != nil {
		return r.CloudFailure(err, path, label+" transcription", ""), nil
	}
	if transcript == "" {
		return Transcript{Error: label + " returned empty transcript", NoSpeech: true, Provider: r.Name}, nil
	}
	return r.Heard(transcript), body
}

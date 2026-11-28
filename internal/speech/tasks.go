package speech

import (
	"context"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/auxiliary"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

// Helper model tasks of speech
type tasks struct{}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{
		{Name: speechconfig.VoiceChat, Title: "Voice chat", About: "The model that answers what you say with voice input", Order: 100, Timeout: 120, Effort: "none"},
		{Name: speechconfig.AudioTags, Title: "Speech tags", About: "The model that adds tone marks to text read aloud", Order: 110, Timeout: 30},
	}
}

// What the rules of a tag rewrite say every time
const (
	tagRules = "Rules:\n- Preserve the spoken words, order, and meaning.\n- Do not add new spoken sentences or remove existing spoken words.\n"
	tagTail  = "- Do not explain or comment.\n- Return only the tagged TTS script."
)

// Text of a voice rewritten by the speech tags model under the system prompt, the fallback when the call fails or comes back empty; a
// failure is logged at the level given
func (j *Job) Tagged(ctx context.Context, system, prompt, fallback, label, instead string, level slog.Level) string {
	if j.Aux == nil {
		return fallback
	}
	temperature := 0.7
	text, err := j.Aux.Complete(ctx, auxiliary.Request{Task: speechconfig.AudioTags, Messages: auxiliary.Prompt(system, prompt), Temperature: &temperature})
	if err != nil {
		slog.Log(ctx, level, label+" audio tag rewrite failed; using "+instead+": "+err.Error())
		return fallback
	}
	if text = unfenced(text); text != "" {
		return text
	}
	return fallback
}

// Rules of a tag rewrite with the tail every prompt ends with
func TagRules(extra string) string {
	return tagRules + extra + tagTail
}

// Text with a code fence around it unwrapped
func unfenced(text string) string {
	text = strings.TrimSpace(text)
	if m, _ := fence.FindStringMatch(text); m != nil {
		return strings.TrimSpace(group(*m, 1))
	}
	return text
}

var fence = pattern("(?s)^```(?:[A-Za-z0-9_-]+)?\\s*(.*?)\\s*```$")

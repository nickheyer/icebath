package speech

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/internal/web"
)

// Key of a service: the variable of the profile's environment, else the account of a provider of a kind the profile connected
func (j *Job) Key(variable string, kinds ...string) string {
	if key := j.Getenv(variable); key != "" {
		return key
	}
	for _, kind := range kinds {
		if credential, ok := web.Credential(j.Speech.providers, j.Speech.credentials, j.Config, kind); ok {
			return strings.TrimSpace(credential.APIKey)
		}
	}
	return ""
}

// Variable OpenAI's audio services prefer over the general OpenAI key, kept apart from routers that take OPENAI_API_KEY
const OpenAIAudioKey = "VOICE_TOOLS_OPENAI_KEY"

// Key of OpenAI's speech and transcription: the voice key, then the OpenAI key or account
func (j *Job) OpenAIKey() string {
	if key := j.Getenv(OpenAIAudioKey); key != "" {
		return key
	}
	return j.Key("OPENAI_API_KEY", "openai-api")
}

// Bearer of direct calls to xAI and where they go: the API key first when preferred, else the Grok sign in first
func (j *Job) XAI(preferKey bool) (web.XAI, bool) {
	return web.XAIBearer(j.Speech.providers, j.Speech.credentials, j.Config, preferKey)
}

// Renews the Grok sign in of the profile after xAI refused a call made with it
func (j *Job) RenewXAI(ctx context.Context) error {
	return web.RenewXAI(ctx, j.Speech.providers, j.Speech.credentials, j.Config)
}

// Message of a service chosen in a section whose credentials don't resolve, naming where the choice is changed
func SelectionError(section, chosen, failure string) string {
	where := "/settings → Speech"
	if section == "stt" {
		where = "/settings → Speech recognition"
	}
	return section + " is configured to use " + chosen + ", but " + failure + ". Pick another one in " + where + "."
}

package contexts

import (
	"errors"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Way a provider refused a request for its size, which a compression may cure
type Refusal string

const (
	// The request is over the model's context window
	Overflow Refusal = "context_overflow"
	// The request body is over what the provider takes
	TooLarge Refusal = "payload_too_large"
	// A long request the account's tier does not cover without extra usage
	LongTier Refusal = "long_context_tier"
	// A picture of the request is over what the provider takes
	ImageTooLarge Refusal = "image_too_large"
)

var (
	overflowPhrases = []string{
		"context length", "context size", "maximum context", "token limit", "too many tokens",
		"reduce the length", "exceeds the limit", "context window", "prompt is too long",
		"prompt exceeds max length", "max_tokens", "maximum number of tokens",
		"exceeds the max_model_len", "max_model_len", "prompt length", "input is too long", "maximum model length",
		"context length exceeded", "truncating input",
		"slot context", "n_ctx_slot",
		"超过最大长度", "上下文长度",
		"tokens in request more than max tokens allowed",
		"max input token", "input token", "exceeds the maximum number of input tokens",
		"maximum allowed input length",
	}
	tooLargePhrases = []string{
		"request entity too large", "payload too large", "error code: 413", "request_too_large",
		"request exceeds the maximum size",
	}
	imagePhrases = []string{
		"image exceeds", "image too large", "image_too_large", "image size exceeds", "image dimensions exceed",
		"dimensions exceed max allowed size", "max allowed size: 8000", "media exceeds", "media too large",
		"patches after processing", "make sure your payload is below", "streamreadconstraints.getmaxstringlength",
	}
	disconnectPhrases = []string{
		"server disconnected", "peer closed connection", "connection reset by peer", "connection was closed",
		"network connection lost", "unexpected eof", "incomplete chunked read",
	}
	// A local server out of memory for the prompt, which a compression does not cure
	memoryPhrases = []string{
		"memory guard", "memory limit exceeded", "memory_guard_tier", "dynamic ceiling",
		"memory ceiling", "available memory", "out of memory", "insufficient memory",
		"prefill would require", "predicted peak would", "prefill safety cap", "metal_cap",
	}
	// Refusals other than those of a bad request that name the overload, the bill or the rate before any size
	busyPhrases = []string{
		"overloaded", "server overload", "at capacity", "over capacity", "credits", "insufficient funds", "insufficient_quota",
		"billing", "payment required", "quota exceeded", "rate limit", "rate_limit", "too many requests", "usage limit",
	}
)

// Size of the request a refusal is weighed against: its tokens, the model's window and its messages
type Load struct {
	Tokens   int
	Window   int
	Messages int
}

// Whether the session is large enough for a vague refusal to be read as one of its size
func (l Load) large(share float64, tokens, messages int) bool {
	return l.Window > 0 && float64(l.Tokens) > float64(l.Window)*share ||
		l.Window <= 256000 && (l.Tokens > tokens || l.Messages > messages)
}

func containsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// How the provider refused the request for its size, empty for a failure of anything else
func Refused(err error, load Load) Refusal {
	failure, ok := errors.AsType[*provider.APIError](err)
	if !ok {
		return ""
	}
	// A plugin's classification decides whether a compression may cure the failure, and how the size refused it
	if failure.Compress != nil && !*failure.Compress {
		return ""
	}
	switch refusal := Refusal(failure.Reason); refusal {
	case Overflow, TooLarge, LongTier, ImageTooLarge:
		return refusal
	}
	if failure.Compress != nil {
		return Overflow
	}
	message := strings.ToLower(failure.Message)
	status := failure.StatusCode
	switch {
	case containsAny(message, imagePhrases):
		return ImageTooLarge
	case containsAny(message, tooLargePhrases) || status == 413:
		return TooLarge
	case status == 429 && strings.Contains(message, "extra usage") && strings.Contains(message, "long context"):
		return LongTier
	case containsAny(message, memoryPhrases) || failure.Kind == provider.ErrAuth:
		return ""
	case status != 400 && (status == 429 || failure.Kind == provider.ErrRateLimit || containsAny(message, busyPhrases)):
		return ""
	case failure.Kind == provider.ErrContextOverflow || containsAny(message, overflowPhrases):
		return Overflow
	case status == 0 && containsAny(message, disconnectPhrases) && load.large(0.6, 120000, 200):
		return Overflow
	case status == 400 && (len(strings.TrimSpace(message)) < 30 || strings.TrimSpace(message) == "error") && load.large(0.4, 80000, 80):
		return Overflow
	}
	return ""
}

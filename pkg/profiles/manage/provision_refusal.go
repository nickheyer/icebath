package manage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Kinds a create refuses with, a bad request that made nothing, in the order a failure is matched against them
const (
	RefusalExists   = "FileExistsError"
	RefusalNotFound = "FileNotFoundError"
	RefusalValue    = "ValueError"
)

// Longest text and most items a gateway client keeps of an error's data, longer strings and lists dropped whole
const (
	errorDataText  = 200
	errorDataItems = 16
)

// Ending of a refusal text longer than an error's data keeps, the error message still holding all of it
const refusalTruncated = " … [truncated]"

// Text a create refused through a gateway carries when its data names no text
const refusedNothingCreated = "the gateway refused the create; nothing was created"

// Text of a create failure as the command prints it, an operating system error in its errno form
func CreateErrorText(err error) string {
	switch err.(type) {
	case *fs.PathError, *os.LinkError, *os.SyscallError, syscall.Errno, *exec.Error:
		return err.Error()
	}
	return err.Error()
}

// Kind of refusal a create failure is, empty for a failure that may have left a profile behind
func RefusalKind(err error) string {
	var invalid *profiles.ValueError
	var undecodable *jsonutil.SyntaxError
	var syntax *json.SyntaxError
	switch {
	case errors.Is(err, fs.ErrExist):
		return RefusalExists
	case errors.Is(err, fs.ErrNotExist):
		return RefusalNotFound
	case errors.As(err, &invalid), errors.As(err, &undecodable), errors.As(err, &syntax):
		return RefusalValue
	}
	return ""
}

// Text with every credential shape and URL credential masked, the form a create failure leaves this process in
func RedactCreateText(text string) string {
	return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true, URLCredentials: true})
}

// Message and data of the error a failed create answers with, the whole redacted text and its kind with the text again in the short parts a client keeps
func Refusal(err error) (string, map[string]any) {
	text := RedactCreateText(CreateErrorText(err))
	kind := RefusalKind(err)
	if kind == "" {
		kind = fmt.Sprintf("%T", err)
	}
	shown := []rune(text)
	if kept := errorDataText * errorDataItems; len(shown) > kept {
		shown = append(shown[:kept-len([]rune(refusalTruncated)):kept-len([]rune(refusalTruncated))], []rune(refusalTruncated)...)
	}
	parts := []any{}
	for start := 0; start < len(shown); start += errorDataText {
		parts = append(parts, string(shown[start:min(start+errorDataText, len(shown))]))
	}
	return text, map[string]any{"type": kind, "message_parts": parts}
}

// Text the data of a failed create carries, redacted again here, empty when it carries none
func MessageFrom(data map[string]any) string {
	parts, isList := data["message_parts"].([]any)
	if !isList {
		return ""
	}
	var text strings.Builder
	for _, part := range parts {
		if piece, isText := part.(string); isText {
			text.WriteString(piece)
		}
	}
	return RedactCreateText(text.String())
}

// Refusal the data of a failed create names, carrying its text, nil when it names none
func RefusalFrom(data map[string]any) error {
	message := MessageFrom(data)
	if message == "" {
		message = refusedNothingCreated
	}
	switch data["type"] {
	case RefusalExists:
		return &profiles.ExistsError{Msg: message}
	case RefusalNotFound:
		return &profiles.NotFoundError{Msg: message}
	case RefusalValue:
		return &profiles.ValueError{Msg: message}
	}
	return nil
}

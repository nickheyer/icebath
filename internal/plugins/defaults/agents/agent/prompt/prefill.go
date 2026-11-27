package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

var roles = map[string]sessionsv1.Role{
	"system":    sessionsv1.Role_ROLE_SYSTEM,
	"user":      sessionsv1.Role_ROLE_USER,
	"assistant": sessionsv1.Role_ROLE_ASSISTANT,
	"tool":      sessionsv1.Role_ROLE_TOOL,
}

// Example messages of the prefill messages file, sent after the system prompt of every request, and what kept them from being read: a
// JSON array of messages with a role and content, and tool_calls or tool_call_id. The file the session's client names wins over the
// setting's; a relative path starts from the profile folder
func Prefill(settings *config.Config, client *tools.Client) ([]*sessionsv1.Message, error) {
	path := strings.TrimSpace(settings.GetString(agentconfig.PrefillMessagesFile))
	if named, _ := client.Lookup("ICEBATH_PREFILL_MESSAGES_FILE"); client != nil && strings.TrimSpace(named) != "" {
		path = strings.TrimSpace(named)
	}
	if path == "" {
		return nil, nil
	}
	if path = fsx.ExpandHome(path); !filepath.IsAbs(path) {
		path = filepath.Join(settings.ProfileDir(settings.Profile()), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("⚠ The example messages file %s could not be read, so the agent starts without it. %s", path, cause(err, "read"))
	}
	var rows []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Calls   []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		CallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &rows); err != nil {
		return nil, fmt.Errorf("⚠ The example messages file %s is not a JSON list of messages, so the agent starts without it. The problem: %s.", path,
			strings.TrimSuffix(err.Error(), "."))
	}
	messages := make([]*sessionsv1.Message, 0, len(rows))
	for i, row := range rows {
		role, ok := roles[row.Role]
		if !ok {
			return nil, fmt.Errorf("⚠ Message %d of the example messages file %s has the role %q, but it must be system, user, assistant or tool.", i+1,
				path, row.Role)
		}
		message := &sessionsv1.Message{Role: role, Text: content(row.Content), ToolCallId: row.CallID}
		for _, call := range row.Calls {
			message.ToolCalls = append(message.ToolCalls, &sessionsv1.ToolCall{Id: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// Text of a message's content: a string, or the text parts of a list of parts
func content(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var texts []string
	for _, part := range parts {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// Why a file could not be read or written, as a sentence
func cause(err error, doing string) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "It does not exist."
	case errors.Is(err, fs.ErrPermission):
		return "You don't have permission to " + doing + " it."
	}
	var path *fs.PathError
	if errors.As(err, &path) {
		err = path.Err
	}
	text := strings.TrimSuffix(err.Error(), ".")
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:] + "."
}

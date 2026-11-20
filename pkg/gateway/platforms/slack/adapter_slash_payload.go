package slack

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Raised like Python AttributeError when the slash command field is a truthy value other than a string
type SlashPayloadError struct{ TypeName string }

func (e *SlashPayloadError) Error() string {
	return fmt.Sprintf("'%s' object has no attribute 'lstrip'", e.TypeName)
}

// Name and alias to /command table of the /icebath slash, installed by the process wiring from pkg/commands
var SubcommandMap func(ctx context.Context) map[string]string

// Private copy of the wired subcommand table, panicking when the process wiring left the hook unassigned
func subcommandMapLive(ctx context.Context) map[string]string {
	if SubcommandMap == nil {
		panic("slack.SubcommandMap unassigned: the process wiring installs commands.SlackSubcommandMap over commands.Default and the raw config")
	}
	out := map[string]string{}
	maps.Copy(out, SubcommandMap(ctx))
	return out
}

// Gateway text of a slash payload, native slashes verbatim and /icebath subcommands mapped through the registry
func SlashCommandText(ctx context.Context, command map[string]any) (string, error) {
	slashName := ""
	if v := command["command"]; pyrepr.Truthy(v) {
		s, ok := v.(string)
		if !ok {
			return "", &SlashPayloadError{TypeName: pyrepr.TypeName(v)}
		}
		slashName = pystr.Strip(pystr.LstripChars(s, "/"))
	}
	rawText := ""
	if v := command["text"]; pyrepr.Truthy(v) {
		rawText = pyrepr.Str(v)
	}
	if slashName != "icebath" && slashName != "" {
		if rawText == "" {
			return "/" + slashName, nil
		}
		return "/" + slashName + " " + rawText, nil
	}
	legacyText := pystr.Strip(rawText)
	subcommandMap := subcommandMapLive(ctx)
	subcommandMap["compact"] = "/compress"
	firstWord := ""
	if words := pystr.SplitWhitespace(legacyText, -1); len(words) > 0 {
		firstWord = words[0]
	}
	if mapped, ok := subcommandMap[firstWord]; ok {
		if rest := pystr.Strip(strings.TrimPrefix(legacyText, firstWord)); rest != "" {
			return pystr.Strip(mapped + " " + rest), nil
		}
		return mapped, nil
	}
	if legacyText == "" {
		return "/help", nil
	}
	return legacyText, nil
}

// Thread anchor of a slash payload, thread_ts before message_ts across the payload and its message or container
func SlashThreadID(command map[string]any) *string {
	candidates := []map[string]any{command}
	for _, key := range []string{"message", "container"} {
		if nested, ok := command[key].(map[string]any); ok {
			candidates = append(candidates, nested)
		}
	}
	for _, tsKey := range []string{"thread_ts", "message_ts"} {
		for _, payload := range candidates {
			if value := payload[tsKey]; pyrepr.Truthy(value) {
				s := pyrepr.Str(value)
				return &s
			}
		}
	}
	return nil
}

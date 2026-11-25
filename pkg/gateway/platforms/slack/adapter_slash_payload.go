package slack

import (
	"context"
	"maps"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Refusal of a slash payload whose command field is a non empty value other than a string, naming the kind it holds
type SlashPayloadError struct{ TypeName string }

func (e *SlashPayloadError) Error() string {
	return "slash command field must be a string, got " + e.TypeName
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
	if v := command["command"]; anyutil.IsTrue(v) {
		s, ok := v.(string)
		if !ok {
			return "", &SlashPayloadError{TypeName: anyutil.Kind(v)}
		}
		slashName = strings.TrimSpace(strings.TrimLeft(s, "/"))
	}
	rawText := ""
	if v := command["text"]; anyutil.IsTrue(v) {
		rawText = anyutil.String(v)
	}
	if slashName != "icebath" && slashName != "" {
		if rawText == "" {
			return "/" + slashName, nil
		}
		return "/" + slashName + " " + rawText, nil
	}
	legacyText := strings.TrimSpace(rawText)
	subcommandMap := subcommandMapLive(ctx)
	subcommandMap["compact"] = "/compress"
	firstWord := ""
	if words := strings.Fields(legacyText); len(words) > 0 {
		firstWord = words[0]
	}
	if mapped, ok := subcommandMap[firstWord]; ok {
		if rest := strings.TrimSpace(strings.TrimPrefix(legacyText, firstWord)); rest != "" {
			return strings.TrimSpace(mapped + " " + rest), nil
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
			if value := payload[tsKey]; anyutil.IsTrue(value) {
				s := anyutil.String(value)
				return &s
			}
		}
	}
	return nil
}

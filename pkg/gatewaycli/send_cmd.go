package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	sendUsageExit   = 2
	sendFailureExit = 1
	sendSuccessExit = 0
)

// Usage error of the send command, printed and turned into its exit code by the entry point
type sendExit struct {
	msg  string
	code int
}

func (e *sendExit) Error() string { return e.msg }

// Whether a reader is an interactive terminal
func readerIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && xterm.IsTerminal(f.Fd())
}

// Everything left on a text stream with universal newlines
func readTextStream(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// Message body from the positional, then the file flag, then piped stdin, absent when nothing is available
func readMessageBody(positional, filePath any, stdin io.Reader) (string, bool, error) {
	if pyrepr.Truthy(positional) {
		return pyrepr.Str(positional), true, nil
	}
	if pyrepr.Truthy(filePath) {
		path := pyrepr.Str(filePath)
		if path == "-" {
			text, err := readTextStream(stdin)
			return text, err == nil, err
		}
		text, err := pyfs.ReadTextChars(path, math.MaxInt, true)
		var decode *pyfs.UnicodeDecodeError
		switch {
		case errors.As(err, &decode):
			return "", false, &sendExit{code: sendUsageExit, msg: fmt.Sprintf("icebath send: %s is not a text file. --file reads the "+
				"message *body* (logs, reports, markdown).\n"+
				"To send an image/document/audio file as a native attachment, "+
				"reference it with MEDIA: in the message text instead:\n"+
				"  icebath send --to telegram \"MEDIA:%s\"\n"+
				"  icebath send --to telegram \"optional caption MEDIA:%s\"\n"+
				"Add [[as_document]] to deliver an image as an uncompressed file:\n"+
				"  icebath send --to telegram \"[[as_document]] MEDIA:%s\"", path, path, path, path)}
		case err != nil && pypath.IsOSError(err):
			return "", false, &sendExit{code: sendUsageExit, msg: fmt.Sprintf("icebath send: cannot read %s: %s", path, pypath.OSErrorText(err))}
		case err != nil:
			return "", false, err
		}
		return text, true, nil
	}
	if readerIsTTY(stdin) {
		return "", false, nil
	}
	text, err := readTextStream(stdin)
	if err != nil {
		return "", false, err
	}
	return text, text != "", nil
}

// Mention values that cannot identify a WhatsApp participant
func invalidWhatsappMentions(mentions []string) []string {
	var invalid []string
	for _, mention := range mentions {
		if gateway.NormalizeWhatsAppMentionJID(mention) == "" {
			invalid = append(invalid, mention)
		}
	}
	return invalid
}

// Prints the send tool result in the requested format and returns the exit code, unknown shapes failing
func emitResult(resultJSON string, jsonMode, quiet bool, w, errw io.Writer) (int, error) {
	var payload *pyjson.Object
	if resultJSON == "" {
		payload = pyjson.NewObject()
	} else {
		decoded, err := pyjson.Loads(resultJSON)
		if err != nil {
			decoded = pyjson.ObjectOf("error", "invalid JSON from send_message_tool", "raw", resultJSON)
		}
		obj, ok := decoded.(*pyjson.Object)
		if !ok && !jsonMode {
			return sendFailureExit, &pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
		}
		if !ok {
			text, err := pyjson.Dumps(decoded, pyjson.Indent(2))
			if err != nil {
				return sendFailureExit, err
			}
			fmt.Fprintln(w, text)
			return sendFailureExit, &pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
		}
		payload = obj
	}
	dump := func() error {
		text, err := pyjson.Dumps(payload, pyjson.Indent(2))
		if err != nil {
			return err
		}
		fmt.Fprintln(w, text)
		return nil
	}
	switch {
	case jsonMode:
		if err := dump(); err != nil {
			return sendFailureExit, err
		}
	case quiet:
	case pyrepr.Truthy(payload.Vals["error"]):
		fmt.Fprintf(errw, "icebath send: %s\n", pyrepr.Str(payload.Vals["error"]))
	case pyrepr.Truthy(payload.Vals["success"]):
		note := "sent"
		if pyrepr.Truthy(payload.Vals["note"]) {
			note = pyrepr.Str(payload.Vals["note"])
		}
		fmt.Fprintln(w, note)
	default:
		if err := dump(); err != nil {
			return sendFailureExit, err
		}
	}
	if !pyrepr.Truthy(payload.Vals["error"]) && (pyrepr.Truthy(payload.Vals["skipped"]) || pyrepr.Truthy(payload.Vals["success"])) {
		return sendSuccessExit, nil
	}
	return sendFailureExit, nil
}

// Prints the channel directory merged with configured platforms, filtered to one platform when named
func listTargets(ctx context.Context, h string, platformFilter any, jsonMode bool, w, errw io.Writer) (int, error) {
	raw := gateway.LoadChannelDirectory(ctx)
	platforms := pyjson.NewObject()
	if known, ok := pyjson.AsObject(raw.Vals["platforms"]); ok {
		platforms = known.Copy()
	}
	if cfg, err := gateway.LoadGatewayConfig(ctx); err == nil {
		for _, plat := range cfg.GetConnectedPlatforms(ctx) {
			name := string(plat)
			if name == "local" || name == "api_server" || name == "webhook" {
				continue
			}
			if _, has := platforms.Get(name); !has {
				platforms.Set(name, []any{})
			}
		}
	}
	if pyrepr.Truthy(platformFilter) {
		key := pystr.Lower(pystr.Strip(pyrepr.Str(platformFilter)))
		filtered := pyjson.NewObject()
		for _, name := range platforms.Keys {
			if pystr.Lower(name) == key {
				filtered.Set(name, platforms.Vals[name])
			}
		}
		if filtered.Len() == 0 {
			configured := "(none)"
			if platforms.Len() > 0 {
				names := append([]string{}, platforms.Keys...)
				sort.Strings(names)
				configured = strings.Join(names, ", ")
			}
			fmt.Fprintf(errw, "icebath send: no targets found for platform '%s'. Configured: %s\n", pyrepr.Str(platformFilter), configured)
			return sendFailureExit, nil
		}
		platforms = filtered
	}
	if jsonMode {
		text, err := pyjson.Dumps(pyjson.ObjectOf("platforms", platforms), pyjson.Indent(2),
			pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
		if err != nil {
			return sendFailureExit, err
		}
		fmt.Fprintln(w, text)
		return sendSuccessExit, nil
	}
	if platforms.Len() == 0 {
		root := home.Root()
		fmt.Fprintln(w, "No messaging platforms configured or no channels discovered yet.")
		fmt.Fprintln(w, "Set one up with `icebath gateway setup`, or run the gateway once so")
		fmt.Fprintf(w, "channel discovery can populate %s.\n", pypath.PathJoin(h, "channel_directory.json"))
		rootDirectory := pypath.PathJoin(root, "channel_directory.json")
		if _, err := os.Stat(rootDirectory); home.Key(root) != home.Key(h) && err == nil {
			fmt.Fprintf(w, "A gateway running from %s already has %s; this shell is scoped to profile home %s, which has none.\n",
				pypath.PathStr(root), rootDirectory, pypath.PathStr(h))
		}
		return sendSuccessExit, nil
	}
	if platformFilter == nil {
		text, err := gateway.FormatDirectoryForDisplay(ctx, platforms)
		if err != nil {
			return sendFailureExit, err
		}
		fmt.Fprintln(w, text)
		return sendSuccessExit, nil
	}
	names := append([]string{}, platforms.Keys...)
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "%s:\n", name)
		channels, _ := platforms.Vals[name].([]any)
		if len(channels) == 0 {
			fmt.Fprintln(w, "  (no channels discovered yet)")
			continue
		}
		for _, raw := range channels {
			channel, ok := pyjson.AsObject(raw)
			if !ok {
				return sendFailureExit, &pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
			}
			var label any = "?"
			if value, has := channel.Get("name"); has {
				label = value
			}
			var chatID any = ""
			if pyrepr.Truthy(channel.Vals["id"]) {
				chatID = channel.Vals["id"]
			} else if pyrepr.Truthy(channel.Vals["chat_id"]) {
				chatID = channel.Vals["chat_id"]
			}
			line := fmt.Sprintf("  %s:%s", name, pyrepr.Str(label))
			if pyrepr.Truthy(chatID) && !pyjson.Equal(chatID, label) {
				line += fmt.Sprintf("  [%s]", pyrepr.Str(chatID))
			}
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w)
	}
	return sendSuccessExit, nil
}

// Loads the home dotenv and bridges top level config scalars into the credential environment without overriding
func loadHermesEnv(ctx context.Context, h string) {
	var scope map[string]string
	if provider.IsMultiplexActive(ctx) {
		scope = provider.CurrentSecretScope(ctx)
	}
	if scope == nil {
		envPath := home.EnvPath(h)
		if _, err := os.Stat(envPath); err == nil {
			_ = provider.LoadDotenvFile(envPath, true)
		}
	}
	configPath := home.ConfigPath(h)
	if exists, err := config.ConfigExists(ctx, configPath); err != nil || !exists {
		return
	}
	cfg, err := config.LoadUserEffective(configPath, os.LookupEnv)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(cfg))
	for key := range cfg {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch cfg[key].(type) {
		case string, int, int64, float64, bool:
		default:
			continue
		}
		if scope != nil {
			if _, has := scope[key]; !has {
				scope[key] = pyrepr.Str(cfg[key])
			}
			continue
		}
		if _, has := os.LookupEnv(key); !has {
			os.Setenv(key, pyrepr.Str(cfg[key]))
		}
	}
}

// Mention flag values of a namespace as strings
func sendMentions(v any) []string {
	switch list := v.(type) {
	case []string:
		return append([]string{}, list...)
	case []any:
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = pyrepr.Str(item)
		}
		return out
	}
	return nil
}

// Sends text from a shell to a configured messaging platform, exit 0 ok, 1 delivery error, 2 usage error
func SendCommand(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error) {
	return cmdSend(ctx, h, ns, os.Stdin, w, errw)
}

// The send command reading its piped body from the given input
func cmdSend(ctx context.Context, h string, ns map[string]any, stdin io.Reader, w, errw io.Writer) (int, error) {
	loadHermesEnv(ctx, h)
	jsonMode, quiet := pyrepr.Truthy(ns["json"]), pyrepr.Truthy(ns["quiet"])
	if pyrepr.Truthy(ns["list_targets"]) {
		return listTargets(ctx, h, ns["message"], jsonMode, w, errw)
	}
	usage := func(msg string) (int, error) {
		fmt.Fprintln(errw, msg)
		return sendUsageExit, nil
	}
	target := ""
	if pyrepr.Truthy(ns["to"]) {
		target = pystr.Strip(pyrepr.Str(ns["to"]))
	}
	if target == "" {
		return usage("icebath send: --to PLATFORM[:channel[:thread]] is required\n" +
			"Examples:\n" +
			"  icebath send --to telegram \"hello\"\n" +
			"  icebath send --to discord:#ops --file report.md\n" +
			"  icebath send --list      # list available targets")
	}
	mentions := sendMentions(ns["mentions"])
	platform, _, _ := strings.Cut(target, ":")
	if len(mentions) > 0 && pystr.Lower(pystr.Strip(platform)) != "whatsapp" {
		return usage("icebath send: --mention is only supported for WhatsApp targets.")
	}
	if invalid := invalidWhatsappMentions(mentions); len(invalid) > 0 {
		return usage("icebath send: invalid --mention value(s): " + strings.Join(invalid, ", ") + ". Use a phone number or participant JID.")
	}
	message, present, err := readMessageBody(ns["message"], ns["file"], stdin)
	var exit *sendExit
	if errors.As(err, &exit) {
		fmt.Fprintln(errw, exit.msg)
		return exit.code, nil
	}
	if err != nil {
		return sendFailureExit, err
	}
	if !present || pystr.Strip(message) == "" {
		return usage("icebath send: no message provided. Pass text as a positional " +
			"argument, use --file PATH, or pipe data via stdin.")
	}
	if pyrepr.Truthy(ns["subject"]) {
		message = pyrepr.Str(ns["subject"]) + "\n\n" + pystr.Lstrip(message)
	}
	toolArgs := map[string]any{"action": "send", "target": target, "message": message}
	if len(mentions) > 0 {
		list := make([]any, len(mentions))
		for i, mention := range mentions {
			list[i] = mention
		}
		toolArgs["mentions"] = list
	}
	result, err := messagetools.SendMessageTool(ctx, toolArgs)
	if err != nil {
		return sendFailureExit, err
	}
	return emitResult(result, jsonMode, quiet, w, errw)
}

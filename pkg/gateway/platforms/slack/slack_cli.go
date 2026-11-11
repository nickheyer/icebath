package slack

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Bounds Slack sets on the long app description, in characters
const (
	LongDescriptionMinCharacters = 175
	LongDescriptionMaxCharacters = 4000
)

// Text of the long description file, read with its newlines kept, or the error text the failure prints
func readLongDescription(sourceArg string) (string, string) {
	source, err := pypath.PathExpanduser(sourceArg)
	if err != nil {
		return "", err.Error()
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", pypath.OSErrorText(err)
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return "", err.Error()
	}
	return text, ""
}

// Prints or writes the Slack app manifest JSON, exit code two for an input the manifest refuses
func ManifestCommand(ctx context.Context, ns map[string]any, w, errw io.Writer) (int, error) {
	name, description := "Icebath", "Your Icebath agent on Slack"
	if value := ns["name"]; pyrepr.Truthy(value) {
		name = pyrepr.Str(value)
	}
	if value := ns["description"]; pyrepr.Truthy(value) {
		description = pyrepr.Str(value)
	}
	var longDescription *string
	if value, ok := ns["long_description"].(string); ok {
		longDescription = &value
	}
	slashesOnly := pyrepr.Truthy(ns["slashes_only"])
	fail := func(msg string) (int, error) {
		fmt.Fprintf(errw, "icebath slack manifest: %s\n", msg)
		return 2, nil
	}
	longDescriptionFile, hasFile := ns["long_description_file"].(string)
	if slashesOnly && (longDescription != nil || hasFile) {
		return fail("long description options cannot be used with --slashes-only")
	}
	if hasFile {
		text, problem := readLongDescription(longDescriptionFile)
		if problem != "" {
			return fail(fmt.Sprintf("cannot read long description from %s: %s", longDescriptionFile, problem))
		}
		longDescription = &text
	}
	if longDescription != nil {
		n := utf8.RuneCountInString(*longDescription)
		if n < LongDescriptionMinCharacters {
			return fail(fmt.Sprintf("long description must be at least %d characters (got %d)", LongDescriptionMinCharacters, n))
		}
		if n > LongDescriptionMaxCharacters {
			return fail(fmt.Sprintf("long description must be at most %d characters (got %d)", LongDescriptionMaxCharacters, n))
		}
	}
	messagingExperience := "assistant"
	if pyrepr.Truthy(ns["agent_view"]) {
		messagingExperience = "agent"
	} else if pyrepr.Truthy(ns["no_assistant"]) {
		messagingExperience = "none"
	}
	var manifest any
	if slashesOnly {
		slashes, err := slashCommandEntries(ctx)
		if err != nil {
			return 0, err
		}
		manifest = slashes
	} else {
		full, err := BuildFullManifest(ctx, name, description, true, &messagingExperience, longDescription)
		if err != nil {
			return 0, err
		}
		manifest = full
	}
	payload, err := pyjson.Dumps(manifest, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return 0, err
	}
	payload += "\n"
	writeTarget := ns["write"]
	if writeTarget == nil {
		_, err := io.WriteString(w, payload)
		return 0, err
	}
	target := filepath.Join(home.Dir(ctx), "slack-manifest.json")
	if bare, isBool := writeTarget.(bool); !isBool || !bare {
		expanded, err := pypath.PathExpanduser(pyrepr.Str(writeTarget))
		if err != nil {
			return 0, err
		}
		target = expanded
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return 0, err
	}
	if err := os.WriteFile(target, []byte(payload), 0o666); err != nil {
		return 0, err
	}
	fmt.Fprintf(errw, "Slack manifest written to: %s\n", target)
	fmt.Fprintf(errw, "\nNext steps:\n"+
		"  1. Open https://api.slack.com/apps and pick your Icebath app\n"+
		"     (or create a new one: Create New App → From an app manifest).\n"+
		"  2. Features → App Manifest → paste the contents of\n"+
		"     %s\n"+
		"  3. Save; Slack will prompt to reinstall the app if scopes or\n"+
		"     slash commands changed.\n"+
		"  4. Make sure Socket Mode is enabled and you have a bot token\n"+
		"     (xoxb-...) and app token (xapp-...) configured via\n"+
		"     `icebath setup`.\n\n", target)
	return 0, nil
}

// Runs icebath slack, whose manifest subcommand prints or writes the app manifest
func CmdSlack(ctx context.Context, ns map[string]any, w, errw io.Writer) (int, error) {
	sub := ns["slack_command"]
	if sub == nil || sub == "" {
		fmt.Fprintln(errw, "usage: icebath slack <subcommand>\n"+
			"\n"+
			"subcommands:\n"+
			"  manifest   Generate a Slack app manifest with every gateway\n"+
			"             command registered as a native slash\n"+
			"\n"+
			"Run `icebath slack manifest -h` for details.")
		return 1, nil
	}
	if sub == "manifest" {
		return ManifestCommand(ctx, ns, w, errw)
	}
	fmt.Fprintf(errw, "Unknown slack subcommand: %s\n", pyrepr.Str(sub))
	return 1, nil
}

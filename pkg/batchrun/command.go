package batchrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reasoning effort levels the reasoning_effort flag accepts
var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Text flag of the parsed command line, empty when it was not given
func flagText(ns map[string]any, key string) string {
	if v, ok := ns[key].(string); ok {
		return v
	}
	return ""
}

// Integer flag of the parsed command line, zero when it was not given
func flagInt(ns map[string]any, key string) int {
	switch v := ns[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// Comma separated flag as a list of stripped items, nil when the flag is empty
func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i, part := range parts {
		parts[i] = pystr.Strip(part)
	}
	return parts
}

// Prints every toolset distribution in name order and how to select one
func printDistributions(out io.Writer) {
	fmt.Fprintln(out, "📊 Available Toolset Distributions")
	fmt.Fprintln(out, strings.Repeat("=", 70))
	listed := tools.ListDistributions()
	names := make([]string, 0, len(listed))
	for name := range listed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tools.PrintDistributionInfo(out, name)
	}
	fmt.Fprintln(out, "\n💡 Usage:")
	fmt.Fprintln(out, "  icebath batch --dataset_file=data.jsonl --batch_size=10 \\")
	fmt.Fprintln(out, "                --run_name=my_run --distribution=<name>")
}

// Prefill messages of a JSON file holding a list of role and content objects
func loadPrefillMessages(path string) ([]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New(pypath.OSErrorText(err))
	}
	text, err := pyjson.DecodeBytes(data)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	messages, ok := pyjson.Plain(decoded).([]any)
	if !ok {
		return nil, errNotMessageList
	}
	return messages, nil
}

// Refusal of a prefill file whose JSON is not a list
var errNotMessageList = errors.New("prefill_messages_file must contain a JSON array of messages")

// Command runs icebath batch from its parsed flags and returns the exit status of the run
func Command(ctx context.Context, ns map[string]any, stdout io.Writer) (int, error) {
	out := stdout
	if out == nil || out == io.Writer(os.Stdout) {
		out = processStdout{}
	}
	if pyrepr.Truthy(ns["list_distributions"]) {
		printDistributions(out)
		return 0, nil
	}
	o := Options{
		DatasetFile: flagText(ns, "dataset_file"), BatchSize: flagInt(ns, "batch_size"), RunName: flagText(ns, "run_name"),
		Distribution: flagText(ns, "distribution"), MaxIterations: flagInt(ns, "max_turns"), BaseURL: flagText(ns, "base_url"),
		APIKey: flagText(ns, "api_key"), Model: flagText(ns, "model"), NumWorkers: flagInt(ns, "num_workers"),
		Verbose: pyrepr.Truthy(ns["verbose"]), EphemeralSystemPrompt: flagText(ns, "ephemeral_system_prompt"),
		LogPrefixChars: flagInt(ns, "log_prefix_chars"), ProvidersAllowed: splitCSV(flagText(ns, "providers_allowed")),
		ProvidersIgnored: splitCSV(flagText(ns, "providers_ignored")), ProvidersOrder: splitCSV(flagText(ns, "providers_order")),
		ProviderSort: flagText(ns, "provider_sort"), MaxSamples: flagInt(ns, "max_samples"),
	}
	for _, check := range []struct {
		invalid bool
		message string
	}{
		{o.DatasetFile == "", "--dataset_file is required"},
		{o.BatchSize < 1, "--batch_size must be a positive integer"},
		{o.RunName == "", "--run_name is required"},
	} {
		if check.invalid {
			fmt.Fprintf(out, "❌ Error: %s\n", check.message)
			return 1, nil
		}
	}
	effort := flagText(ns, "reasoning_effort")
	switch {
	case pyrepr.Truthy(ns["reasoning_disabled"]):
		o.ReasoningConfig = map[string]any{"effort": "none"}
		fmt.Fprintln(out, "🧠 Reasoning: DISABLED (effort=none)")
	case effort != "":
		if !slices.Contains(reasoningEfforts, effort) {
			fmt.Fprintf(out, "❌ Error: --reasoning_effort must be one of: %s\n", strings.Join(reasoningEfforts, ", "))
			return 1, nil
		}
		o.ReasoningConfig = map[string]any{"enabled": true, "effort": effort}
		fmt.Fprintf(out, "🧠 Reasoning effort: %s\n", effort)
	}
	if file := flagText(ns, "prefill_messages_file"); file != "" {
		messages, err := loadPrefillMessages(file)
		switch {
		case errors.Is(err, errNotMessageList):
			fmt.Fprintf(out, "❌ Error: %s\n", err.Error())
			return 1, nil
		case err != nil:
			fmt.Fprintf(out, "❌ Error loading prefill messages: %s\n", err.Error())
			return 1, nil
		}
		o.PrefillMessages = messages
		fmt.Fprintf(out, "💬 Loaded %d prefill messages from %s\n", len(messages), file)
	}
	runner, err := New(ctx, o, out)
	if err == nil {
		err = runner.Run(ctx, pyrepr.Truthy(ns["resume"]))
	}
	var interrupted *InterruptedError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &interrupted):
		return interrupted.ExitCode(), nil
	}
	fmt.Fprintf(out, "\n❌ Fatal error: %s\n", err.Error())
	var panicked *promptPanic
	if o.Verbose && errors.As(err, &panicked) {
		fmt.Fprintf(os.Stderr, "%s\n", panicked.stack)
	}
	return 1, nil
}

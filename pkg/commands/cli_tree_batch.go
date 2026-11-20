package commands

import (
	"strings"

	"github.com/spf13/cobra"
)

// Examples and output layout shown under the batch help
const batchEpilog = `Examples:
    icebath batch --dataset_file=data.jsonl --batch_size=10 --run_name=my_run
    icebath batch --dataset_file=data.jsonl --batch_size=10 --run_name=my_run --resume
    icebath batch --dataset_file=data.jsonl --batch_size=10 --run_name=image_test --distribution=image_gen
    icebath batch --dataset_file=data.jsonl --batch_size=10 --run_name=my_run --reasoning_disabled
    icebath batch --dataset_file=data.jsonl --batch_size=10 --run_name=my_run --prefill_messages_file=prefill.json
    icebath batch --list_distributions

Output goes to data/<run_name>/ under the current directory: trajectories.jsonl (every batch merged),
batch_N.jsonl (one file per batch), checkpoint.json and statistics.json.`

// Declares icebath batch with the flag names of its dataset tooling, each underscore name also accepted with dashes
func (c *CLI) addBatch(root *cobra.Command) {
	batch := c.newCmd(root, "batch", "Run the agent over a JSONL prompt dataset in parallel and save the trajectories",
		"Run batch processing of agent prompts from a dataset. Every line of the JSONL dataset is an object\n"+
			"with a 'prompt' field. The prompts are split into batches that run in parallel workers, each prompt\n"+
			"in a fresh agent with toolsets sampled from the chosen distribution. Finished prompts are\n"+
			"checkpointed, so an interrupted run continues with --resume.")
	c.epilog(batch, batchEpilog)
	sf(batch, "dataset_file", "", "", "Path to JSONL file with 'prompt' field in each entry")
	inf(batch, "batch_size", "", 0, "Number of prompts per batch")
	sf(batch, "run_name", "", "", "Name for this run (used for output and checkpointing)")
	sf(batch, "distribution", "", "default", `Toolset distribution to use (default: "default")`)
	sf(batch, "model", "", "anthropic/claude-sonnet-4.6", `Model name to use (default: "anthropic/claude-sonnet-4.6")`)
	sf(batch, "api_key", "", "", "API key for model authentication (default: the credentials of the configured provider)")
	sf(batch, "base_url", "", "https://openrouter.ai/api/v1", "Base URL for model API (default: https://openrouter.ai/api/v1)")
	inf(batch, "max_turns", "", 10, "Maximum number of tool calling iterations per prompt (default: 10)")
	inf(batch, "num_workers", "", 4, "Number of parallel workers (default: 4)")
	bf(batch, "resume", "", "Resume from checkpoint if run was interrupted")
	bf(batch, "verbose", "", "Enable verbose logging")
	bf(batch, "list_distributions", "", "List available toolset distributions and exit")
	sf(batch, "ephemeral_system_prompt", "", "", "System prompt used during agent execution but NOT saved to trajectories")
	inf(batch, "log_prefix_chars", "", 100, "Number of characters to show in log previews for tool calls/responses (default: 100)")
	sf(batch, "providers_allowed", "", "", `Comma-separated list of OpenRouter providers to allow (e.g. "anthropic,openai")`)
	sf(batch, "providers_ignored", "", "", `Comma-separated list of OpenRouter providers to ignore (e.g. "together,deepinfra")`)
	sf(batch, "providers_order", "", "", `Comma-separated list of OpenRouter providers to try in order (e.g. "anthropic,openai,google")`)
	sf(batch, "provider_sort", "", "", `Sort providers by "price", "throughput", or "latency" (OpenRouter only)`)
	sf(batch, "reasoning_effort", "", "", `Reasoning effort: "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra" (default: "medium")`)
	bf(batch, "reasoning_disabled", "", "Completely disable reasoning/thinking tokens")
	sf(batch, "prefill_messages_file", "", "", "Path to JSON file containing prefill messages (list of {role, content} objects)")
	inf(batch, "max_samples", "", 0, "Only process the first N samples from the dataset (default: all of them)")
	noneDefault(batch, "batch_size", "max_samples")
	for _, name := range []string{"dataset_file", "batch_size", "run_name", "api_key", "base_url", "max_turns", "num_workers", "list_distributions",
		"ephemeral_system_prompt", "log_prefix_chars", "providers_allowed", "providers_ignored", "providers_order", "provider_sort",
		"reasoning_effort", "reasoning_disabled", "prefill_messages_file", "max_samples"} {
		c.aliasFlag(batch, strings.ReplaceAll(name, "_", "-"), name)
	}
}

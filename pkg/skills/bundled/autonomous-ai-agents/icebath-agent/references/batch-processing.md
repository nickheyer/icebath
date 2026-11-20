# Batch Processing

`icebath batch` runs the agent across a JSONL dataset of prompts in parallel and writes one trajectory per prompt. It is meant for training data generation and for evaluation: the output is ShareGPT-style conversations with tool usage statistics and reasoning coverage. The command is part of the full build.

## Quick Start

```bash
# Basic batch run
icebath batch \
    --dataset_file=data/prompts.jsonl \
    --batch_size=10 \
    --run_name=my_first_run \
    --model=anthropic/claude-sonnet-4.6 \
    --num_workers=4

# Resume an interrupted run
icebath batch \
    --dataset_file=data/prompts.jsonl \
    --batch_size=10 \
    --run_name=my_first_run \
    --resume

# List available toolset distributions
icebath batch --list_distributions
```

Every flag is also accepted with dashes in place of underscores (`--dataset-file`, `--batch-size`, ...).

## Dataset Format

The input dataset is a JSONL file (one JSON object per line). Each entry must have a `prompt` field:

```jsonl
{"prompt": "Write a Python function that finds the longest palindromic substring"}
{"prompt": "Create a REST API endpoint for user authentication using Flask"}
{"prompt": "Debug this error: TypeError: cannot unpack non-iterable NoneType object"}
```

Blank lines, lines that are not JSON and entries without a `prompt` are skipped with a warning.

Entries can optionally include:
- `image` or `docker_image`: a container image for this prompt's sandbox (Docker, Modal, Daytona and plugin backends such as Singularity)
- `cwd`: working directory override for the task's terminal session

## Options

| Flag | Default | Description |
|------|---------|-------------|
| `--dataset_file` | (required) | Path to the JSONL dataset |
| `--batch_size` | (required) | Prompts per batch |
| `--run_name` | (required) | Name of the run (output directory and checkpoint) |
| `--distribution` | `default` | Toolset distribution to sample from |
| `--model` | `anthropic/claude-sonnet-4.6` | Model to use |
| `--base_url` | `https://openrouter.ai/api/v1` | API base URL |
| `--api_key` | (configured provider) | API key for the model |
| `--max_turns` | `10` | Tool-calling iterations per prompt |
| `--num_workers` | `4` | Parallel workers |
| `--resume` | off | Skip prompts the batch files already hold |
| `--verbose` | off | Verbose logging |
| `--max_samples` | all | Only the first N dataset entries |

With `--base_url` and `--api_key` both given, the run talks to that endpoint directly. Without `--api_key` the provider configured in `config.yaml` (`icebath model`) supplies the route and its credentials, and `--model` still names the model.

### Provider Routing (OpenRouter)

| Flag | Description |
|------|-------------|
| `--providers_allowed` | Comma-separated providers to allow (e.g. `"anthropic,openai"`) |
| `--providers_ignored` | Comma-separated providers to ignore (e.g. `"together,deepinfra"`) |
| `--providers_order` | Comma-separated preferred provider order |
| `--provider_sort` | Sort by `"price"`, `"throughput"`, or `"latency"` |

### Reasoning Control

| Flag | Description |
|------|-------------|
| `--reasoning_effort` | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra` |
| `--reasoning_disabled` | Disable reasoning/thinking tokens (wins over `--reasoning_effort`) |

### Advanced Options

| Flag | Description |
|------|-------------|
| `--ephemeral_system_prompt` | System prompt used while the agent runs but never saved to a trajectory |
| `--log_prefix_chars` | Characters shown in log previews (default: 100) |
| `--prefill_messages_file` | JSON file with a list of `{role, content}` messages prepended for few-shot priming |

## Toolset Distributions

Each prompt gets a set of toolsets sampled from a **distribution**, so the data covers varied tool combinations. `--list_distributions` prints all of them.

A distribution gives each toolset a probability. The sampler rolls each entry on its own (a `a+b` entry rolls once for both toolsets) and enables the most likely entry when nothing was rolled. A toolset this build does not carry is skipped with a warning.

## How a Run Works

- The dataset is split into batches of `--batch_size`. `--num_workers` workers take batches from a queue; a worker runs the prompts of its batch one after another.
- Every prompt gets a fresh agent, its own task id (`task_<index>`, so its own terminal session or sandbox), no project context files and no persistent memory. The agent is closed when the prompt ends.
- A finished prompt is appended to `batch_<n>.jsonl` and synced to disk before it counts as done.
- After each batch the checkpoint is rewritten. A progress bar shows finished batches.
- Ctrl+C stops the workers and exits with status 130. Prompts that were running are not recorded and run again on `--resume`.

## Output

Everything goes to `data/<run_name>/` under the current directory:

```text
data/my_run/
├── trajectories.jsonl    # every batch merged
├── batch_0.jsonl         # one file per batch
├── batch_1.jsonl
├── checkpoint.json       # completed prompt indices
└── statistics.json       # tool usage and reasoning totals
```

Each line of `trajectories.jsonl`:

```json
{
  "prompt_index": 42,
  "conversations": [
    {"from": "system", "value": "You are a function calling AI model. ..."},
    {"from": "human", "value": "Write a function..."},
    {"from": "gpt", "value": "<think>\n...\n</think>\n<tool_call>\n{\"name\": \"terminal\", \"arguments\": {...}}\n</tool_call>"},
    {"from": "tool", "value": "<tool_response>\n{\"tool_call_id\": \"...\", \"name\": \"terminal\", \"content\": ...}\n</tool_response>"},
    {"from": "gpt", "value": "<think>\n...\n</think>\nHere's the completed function..."}
  ],
  "metadata": {"batch_num": 2, "timestamp": "2026-01-15T10:30:00", "model": "anthropic/claude-sonnet-4.6"},
  "completed": true,
  "partial": false,
  "api_calls": 3,
  "toolsets_used": ["terminal", "file"],
  "tool_stats": {"terminal": {"count": 2, "success": 2, "failure": 0}, "read_file": {"count": 1, "success": 1, "failure": 0}},
  "tool_error_counts": {"terminal": 0, "read_file": 0}
}
```

`tool_stats` and `tool_error_counts` list every registered tool with zero defaults, so all rows share one schema (HuggingFace datasets need that). Secrets in prompts, tool arguments and tool output are redacted before a row is written.

`statistics.json` holds the run name, distribution, model, prompt and batch counts, duration, per tool `count` / `success` / `failure` / `success_rate` / `failure_rate`, the reasoning turn counts and the number of discarded samples.

## Checkpoint and Resume

- `checkpoint.json` is rewritten after each batch with the indices of finished prompts.
- `--resume` scans every `batch_*.jsonl` and matches finished prompts by their text, so it still works when the dataset was reordered. The remaining prompts are batched again and appended to the batch files.
- Failed prompts are not marked as done and run again on resume.
- At the end all batch files, old and new, are merged into `trajectories.jsonl`.

## Quality Filtering

- **No reasoning:** a sample where no assistant turn has reasoning (no `<REASONING_SCRATCHPAD>` and no native thinking) is discarded. A short tombstone row keeps resume from running it again; tombstones never reach `trajectories.jsonl`, and the tool calls of a discarded sample are not counted.
- **Corrupted entries:** rows naming a tool that is not registered, and lines that are not JSON, are left out of the merge.

## Per-Prompt Container Images

```jsonl
{"prompt": "Install numpy and compute eigenvalues of a 3x3 matrix", "image": "python:3.11-slim"}
{"prompt": "Compile this Rust program and run it", "image": "rust:1.75"}
{"prompt": "Set up a Node.js Express server", "image": "node:20-alpine", "cwd": "/app"}
```

With the Docker terminal backend the image is checked locally and pulled if needed before the prompt runs; a prompt whose image cannot be pulled fails without spending tokens.

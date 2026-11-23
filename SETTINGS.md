# Settings reference

Every icebath setting, grouped as in the terminal UI's /settings screen. Change one there, with `icebath config set <key> <value>`, or in `config.yaml`.

Generated from `pkg/settings` by `go generate ./pkg/settings`; edits here are overwritten.

## Model

The model, how hard it thinks, and its fallback.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Model | `model.default` | `''` | Which model answers your prompts. /model changes it for one session or saves it here. Applies to new sessions. |
| Provider | `model.provider` | `''` | The service the model is reached through. Applies to new sessions. |
| Reasoning effort | `agent.reasoning_effort` | unset (the model's default) | How long the model thinks before answering. More effort is slower and costs more, but is often better. Models that cannot think ignore it. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. Applies from the next turn. |
| Fast mode | `agent.service_tier` | see config | Pay more for priority processing so replies come back faster. Auto is fast for the first seconds of each turn, cold only on a session's first turn. Only some models offer it. One of: normal, fast, ultrafast, auto, cold. Applies from the next turn. |
| Fallback models | `fallback_providers` | `[]` | Models to try, in order, when the main one fails. Applies from the next turn. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Logins per provider | `logins` | `{}` | Which login each provider uses and what happens when it runs out. Manage logins in /logins; a session can still pin its own. Applies to new sessions. |
| Endpoint address | `model.base_url` | `''` | Custom address of the model's service. Empty uses the provider's own address. Applies to new sessions. |
| Request format | `model.api_mode` | `''` | Request format the endpoint speaks. Empty picks it from the provider. Applies to new sessions. |
| Codex research access | `agent.daybreak` | see config | Ask Codex for its security research access in your own sessions. Only your own terminal sessions send it, and only on a Codex subscription that offers it. Applies from the next turn. |
| Fast mode window | `agent.fast_auto_seconds` | see config | How long Fast mode stays on after it switches on by itself. |
| Reasoning by model | `agent.reasoning_overrides` | `{}` | Reasoning effort for particular models, by model name. Models not listed use Reasoning effort. |
| Answer length | `agent.text_verbosity` | unset (the model's default) | How wordy answers should be, for models that support it. One of: low, medium, high. Unset: the model's default. |
| Approval key | `auxiliary.approval.api_key` | secret, in .env | API key for smart command approval. Empty uses your normal login. |
| Approval URL | `auxiliary.approval.base_url` | `''` | Custom endpoint address for smart command approval. |
| Approval fields | `auxiliary.approval.extra_body` | `{}` | Extra request fields for smart command approval. |
| Approval model | `auxiliary.approval.model` | `''` | Model for smart command approval. Empty picks one for you. |
| Approval provider | `auxiliary.approval.provider` | see config | Provider for smart command approval. Auto picks one for you. |
| Approval thinking | `auxiliary.approval.reasoning_effort` | unset (the model's default) | Thinking effort for smart command approval. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Approval timeout | `auxiliary.approval.timeout` | see config | Time limit for smart command approval. |
| Auto review key | `auxiliary.background_review.api_key` | secret, in .env | API key for background reviews. Empty uses your normal login. |
| Auto review URL | `auxiliary.background_review.base_url` | `''` | Custom endpoint address for background reviews. |
| Auto review | `auxiliary.background_review.enabled` | see config | Let the model review sessions in the background automatically. |
| Auto review fields | `auxiliary.background_review.extra_body` | `{}` | Extra request fields for background reviews. |
| Auto review model | `auxiliary.background_review.model` | `''` | Model for background reviews. Empty picks one for you. |
| Auto review provider | `auxiliary.background_review.provider` | see config | Provider for background reviews. Auto picks one for you. |
| Auto review thinking | `auxiliary.background_review.reasoning_effort` | unset (the model's default) | Thinking effort for background reviews. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Auto review timeout | `auxiliary.background_review.timeout` | see config | Time limit for background reviews. |
| Compression key | `auxiliary.compression.api_key` | secret, in .env | API key for conversation summaries. Empty uses your normal login. |
| Compression URL | `auxiliary.compression.base_url` | `''` | Custom endpoint address for conversation summaries. |
| Compression fields | `auxiliary.compression.extra_body` | `{}` | Extra request fields for conversation summaries. |
| Compression model | `auxiliary.compression.model` | `''` | Model for conversation summaries. Empty picks one for you. |
| Stall limit | `auxiliary.compression.no_progress_timeout` | unset (built-in 60 seconds) | Give up a summary stream that stops sending data after this long. Unset: built-in 60 seconds. |
| Compression provider | `auxiliary.compression.provider` | see config | Provider for conversation summaries. Auto picks one for you. |
| Compression thinking | `auxiliary.compression.reasoning_effort` | unset (the model's default) | Thinking effort for conversation summaries. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Compression timeout | `auxiliary.compression.timeout` | see config | Time limit for conversation summaries. Never less than the built-in minimum. |
| Curator key | `auxiliary.curator.api_key` | secret, in .env | API key for skill upkeep. Empty uses your normal login. |
| Curator URL | `auxiliary.curator.base_url` | `''` | Custom endpoint address for skill upkeep. |
| Curator fields | `auxiliary.curator.extra_body` | `{}` | Extra request fields for skill upkeep. |
| Curator model | `auxiliary.curator.model` | `''` | Model for skill upkeep. Empty picks one for you. |
| Curator provider | `auxiliary.curator.provider` | see config | Provider for skill upkeep. Auto picks one for you. |
| Curator thinking | `auxiliary.curator.reasoning_effort` | unset (the model's default) | Thinking effort for skill upkeep. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Curator timeout | `auxiliary.curator.timeout` | see config | Time limit for skill upkeep. |
| Free models only | `auxiliary.free_only` | see config | Only use free models for the OpenRouter fallback. |
| Goal judge key | `auxiliary.goal_judge.api_key` | secret, in .env | API key for goal checks. Empty uses your normal login. |
| Goal judge URL | `auxiliary.goal_judge.base_url` | `''` | Custom endpoint address for goal checks. |
| Goal judge fields | `auxiliary.goal_judge.extra_body` | `{}` | Extra request fields for goal checks. |
| Goal judge model | `auxiliary.goal_judge.model` | `''` | Model for goal checks. Empty picks one for you. |
| Goal judge provider | `auxiliary.goal_judge.provider` | see config | Provider for goal checks. Auto picks one for you. |
| Goal judge thinking | `auxiliary.goal_judge.reasoning_effort` | unset (the model's default) | Thinking effort for goal checks. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Goal judge timeout | `auxiliary.goal_judge.timeout` | see config | Time limit for goal checks. |
| Decomposer key | `auxiliary.kanban_decomposer.api_key` | secret, in .env | API key for splitting kanban tasks. Empty uses your normal login. |
| Decomposer URL | `auxiliary.kanban_decomposer.base_url` | `''` | Custom endpoint address for splitting kanban tasks. |
| Decomposer fields | `auxiliary.kanban_decomposer.extra_body` | `{}` | Extra request fields for splitting kanban tasks. |
| Decomposer model | `auxiliary.kanban_decomposer.model` | `''` | Model for splitting kanban tasks. Empty picks one for you. |
| Decomposer provider | `auxiliary.kanban_decomposer.provider` | see config | Provider for splitting kanban tasks. Auto picks one for you. |
| Decomposer thinking | `auxiliary.kanban_decomposer.reasoning_effort` | unset (the model's default) | Thinking effort for splitting kanban tasks. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Decomposer timeout | `auxiliary.kanban_decomposer.timeout` | see config | Time limit for splitting kanban tasks. |
| MCP key | `auxiliary.mcp.api_key` | secret, in .env | API key for MCP tool reasoning. Empty uses your normal login. |
| MCP URL | `auxiliary.mcp.base_url` | `''` | Custom endpoint address for MCP tool reasoning. |
| MCP fields | `auxiliary.mcp.extra_body` | `{}` | Extra request fields for MCP tool reasoning. |
| MCP model | `auxiliary.mcp.model` | `''` | Model for MCP tool reasoning. Empty picks one for you. |
| MCP provider | `auxiliary.mcp.provider` | see config | Provider for MCP tool reasoning. Auto picks one for you. |
| MCP thinking | `auxiliary.mcp.reasoning_effort` | unset (the model's default) | Thinking effort for MCP tool reasoning. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| MCP timeout | `auxiliary.mcp.timeout` | see config | Time limit for MCP tool reasoning. |
| Memory query key | `auxiliary.memory_query_rewrite.api_key` | secret, in .env | API key for memory search rewriting. Empty uses your normal login. |
| Memory query URL | `auxiliary.memory_query_rewrite.base_url` | `''` | Custom endpoint address for memory search rewriting. |
| Memory query fields | `auxiliary.memory_query_rewrite.extra_body` | `{}` | Extra request fields for memory search rewriting. |
| Memory query model | `auxiliary.memory_query_rewrite.model` | `''` | Model for memory search rewriting. Empty picks one for you. |
| Memory query provider | `auxiliary.memory_query_rewrite.provider` | see config | Provider for memory search rewriting. Auto picks one for you. |
| Memory query timeout | `auxiliary.memory_query_rewrite.timeout` | see config | Time limit for memory search rewriting. |
| Aggregator key | `auxiliary.moa_aggregator.api_key` | secret, in .env | API key for mixture final answers. Empty uses your normal login. |
| Aggregator URL | `auxiliary.moa_aggregator.base_url` | `''` | Custom endpoint address for mixture final answers. |
| Aggregator fields | `auxiliary.moa_aggregator.extra_body` | `{}` | Extra request fields for mixture final answers. |
| Aggregator model | `auxiliary.moa_aggregator.model` | `''` | Model for mixture final answers. Empty picks one for you. |
| Aggregator provider | `auxiliary.moa_aggregator.provider` | see config | Provider for mixture final answers. Auto picks one for you. |
| Aggregator timeout | `auxiliary.moa_aggregator.timeout` | see config | Time limit for mixture final answers. |
| Reference key | `auxiliary.moa_reference.api_key` | secret, in .env | API key for mixture reference answers. Empty uses your normal login. |
| Reference URL | `auxiliary.moa_reference.base_url` | `''` | Custom endpoint address for mixture reference answers. |
| Reference fields | `auxiliary.moa_reference.extra_body` | `{}` | Extra request fields for mixture reference answers. |
| Reference model | `auxiliary.moa_reference.model` | `''` | Model for mixture reference answers. Empty picks one for you. |
| Reference provider | `auxiliary.moa_reference.provider` | see config | Provider for mixture reference answers. Auto picks one for you. |
| Reference timeout | `auxiliary.moa_reference.timeout` | see config | Time limit for mixture reference answers. |
| Monitor key | `auxiliary.monitor.api_key` | secret, in .env | API key for job item urgency scoring. Empty uses your normal login. |
| Monitor URL | `auxiliary.monitor.base_url` | `''` | Custom endpoint address for job item urgency scoring. |
| Monitor fields | `auxiliary.monitor.extra_body` | `{}` | Extra request fields for job item urgency scoring. |
| Monitor model | `auxiliary.monitor.model` | `''` | Model for job item urgency scoring. Empty picks one for you. |
| Monitor provider | `auxiliary.monitor.provider` | see config | Provider for job item urgency scoring. Auto picks one for you. |
| Monitor thinking | `auxiliary.monitor.reasoning_effort` | unset (the model's default) | Thinking effort for job item urgency scoring. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Monitor timeout | `auxiliary.monitor.timeout` | see config | Time limit for job item urgency scoring. |
| Fallback free model | `auxiliary.openrouter_model` | `''` | Free OpenRouter model tried when no helper model is set up. Empty uses a built-in free model. |
| Profile text key | `auxiliary.profile_describer.api_key` | secret, in .env | API key for profile descriptions. Empty uses your normal login. |
| Profile text URL | `auxiliary.profile_describer.base_url` | `''` | Custom endpoint address for profile descriptions. |
| Profile text fields | `auxiliary.profile_describer.extra_body` | `{}` | Extra request fields for profile descriptions. |
| Profile text model | `auxiliary.profile_describer.model` | `''` | Model for profile descriptions. Empty picks one for you. |
| Profile text provider | `auxiliary.profile_describer.provider` | see config | Provider for profile descriptions. Auto picks one for you. |
| Profile text thinking | `auxiliary.profile_describer.reasoning_effort` | unset (the model's default) | Thinking effort for profile descriptions. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Profile text timeout | `auxiliary.profile_describer.timeout` | see config | Time limit for profile descriptions. |
| Review key | `auxiliary.review.api_key` | secret, in .env | API key for the review command. Empty uses your normal login. |
| Review API style | `auxiliary.review.api_mode` | `''` | Request format of the review model. Empty picks it for you. |
| Review URL | `auxiliary.review.base_url` | `''` | Custom endpoint address for the review command. |
| Review model | `auxiliary.review.model` | `''` | Model for the review command. Empty picks one for you. |
| Review provider | `auxiliary.review.provider` | see config | Provider for the review command. Auto picks one for you. |
| Streaming-only hosts | `auxiliary.stream_only_base_urls` | `[]` | Endpoint addresses that only work with streamed replies. Any address containing one of these texts is requested in streaming mode. |
| Title key | `auxiliary.title_generation.api_key` | secret, in .env | API key for session titles. Empty uses your normal login. |
| Title URL | `auxiliary.title_generation.base_url` | `''` | Custom endpoint address for session titles. |
| Auto titles | `auxiliary.title_generation.enabled` | see config | Name new sessions automatically. |
| Title fields | `auxiliary.title_generation.extra_body` | `{}` | Extra request fields for session titles. |
| Title language | `auxiliary.title_generation.language` | `''` | Language of session titles. Empty matches the language of your message. |
| Title model | `auxiliary.title_generation.model` | `''` | Model for session titles. Empty picks one for you. |
| Better titles | `auxiliary.title_generation.model_upgrade_enabled` | see config | Replace the quick title with one written by the title model. |
| Prefer fast model | `auxiliary.title_generation.prefer_fast_model` | see config | Pick a quicker model for titles when one is available. |
| Title provider | `auxiliary.title_generation.provider` | see config | Provider for session titles. Auto picks one for you. |
| Title thinking | `auxiliary.title_generation.reasoning_effort` | unset (the model's default) | Thinking effort for session titles. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Title timeout | `auxiliary.title_generation.timeout` | see config | Time limit for session titles. |
| Helper retries | `auxiliary.transient_retries` | see config | Retries for helper calls after a brief network error. Between 0 and 6. |
| Triage key | `auxiliary.triage_specifier.api_key` | secret, in .env | API key for kanban task specs. Empty uses your normal login. |
| Triage URL | `auxiliary.triage_specifier.base_url` | `''` | Custom endpoint address for kanban task specs. |
| Triage fields | `auxiliary.triage_specifier.extra_body` | `{}` | Extra request fields for kanban task specs. |
| Triage model | `auxiliary.triage_specifier.model` | `''` | Model for kanban task specs. Empty picks one for you. |
| Triage provider | `auxiliary.triage_specifier.provider` | see config | Provider for kanban task specs. Auto picks one for you. |
| Triage thinking | `auxiliary.triage_specifier.reasoning_effort` | unset (the model's default) | Thinking effort for kanban task specs. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Triage timeout | `auxiliary.triage_specifier.timeout` | see config | Time limit for kanban task specs. |
| Vision key | `auxiliary.vision.api_key` | secret, in .env | API key for image analysis. Empty uses your normal login. |
| Vision URL | `auxiliary.vision.base_url` | `''` | Custom endpoint address for image analysis. |
| Download timeout | `auxiliary.vision.download_timeout` | see config | How long to wait when downloading an image or media file. |
| Vision fields | `auxiliary.vision.extra_body` | `{}` | Extra request fields for image analysis. |
| Vision model | `auxiliary.vision.model` | `''` | Model for image analysis. Empty picks one for you. |
| Vision provider | `auxiliary.vision.provider` | see config | Provider for image analysis. Auto picks one for you. |
| Vision thinking | `auxiliary.vision.reasoning_effort` | unset (the model's default) | Thinking effort for image analysis. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Vision timeout | `auxiliary.vision.timeout` | see config | Time limit for image analysis. |
| Video model | `auxiliary.video.model` | unset (the image analysis model) | Model that watches videos for the agent. Unset: the image analysis model. Applies from the next turn. |
| Guardrail ID | `bedrock.guardrail.guardrail_identifier` | `''` | ID of the Bedrock guardrail to apply. Both the ID and the version must be set for the guardrail to apply. |
| Image workers | `auxiliary.vision.max_concurrency` | unset (one per processor core) | How many images are prepared at the same time. Unset: one per processor core. Restart the background service to apply. |
| Guardrail version | `bedrock.guardrail.guardrail_version` | `''` | Version of the Bedrock guardrail to apply. |
| Vision temperature | `auxiliary.vision.temperature` | see config | How varied image and video descriptions may be. Low values give steadier descriptions. Applies from the next turn. |
| Guardrail streaming | `bedrock.guardrail.stream_processing_mode` | see config | Whether guardrail checks block streamed replies (sync) or not. One of: async, sync. |
| Guardrail trace | `bedrock.guardrail.trace` | see config | Whether Bedrock reports why a guardrail acted. One of: disabled, enabled, enabled_full. |
| Bedrock region | `bedrock.region` | `''` | AWS region used for Bedrock requests. Blank takes the region from the endpoint address, else us-east-1. |
| Stay-put window | `fallback.min_switch_reset_seconds` | see config | Stay on the main model if its rate limit ends within this time. 0 turns this off, so any rate limit switches to the fallback. |
| Model overrides | `model_overrides` | `{}` | Per-model limits such as context size, by provider. |
| Min coding score | `openrouter.min_coding_score` | see config | Lowest coding score for automatic OpenRouter model routing. |
| Response cache | `openrouter.response_cache` | see config | Let OpenRouter reuse identical earlier answers. |
| Cache lifetime | `openrouter.response_cache_ttl` | see config | How long OpenRouter keeps cached answers. |
| Prompt cache time | `prompt_caching.cache_ttl` | see config | How long the provider keeps your prompt cached. |
| Providers | `providers` | `{}` | Your own named model endpoints and their settings. |
| Vertex project | `vertex.project_id` | `''` | Google Cloud project used for Vertex AI requests. |
| Vertex region | `vertex.region` | see config | Google Cloud region used for Vertex AI requests. |
| OpenRouter routing | `provider_routing` | `{}` | Which upstream providers OpenRouter may send your requests to. Keys such as only, ignore, order and sort, optionally under a model name. Applies from the next turn. |
| Extra headers | `model.default_headers` | `{}` | Extra HTTP headers sent with every request to the model. Header names map to values. A header set here replaces the built-in one. Applies to new sessions. |
| Azure sign-in | `model.auth_mode` | see config | How icebath signs in to Azure Foundry. One of: api_key, entra_id. Applies to new sessions. |
| Entra options | `model.entra` | `{}` | Options of the Microsoft Entra sign-in, such as the token scope. Applies to new sessions. |
| Older endpoints | `custom_providers` | `[]` | Older list form of your own model endpoints. Providers holds new endpoints. Entries here still work. Applies to new sessions. |
| Context window | `model.context_length` | unset (the size icebath detects) | How many tokens the main model can hold, when icebath guesses wrong. Unset uses the size the provider reports or the built-in table. Unset: the size icebath detects. Applies to new sessions. |

## Agent

How long the agent works and when it shortens chats.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Personality | `display.personality` | `''` | Tone and style preset the agent speaks with. Applies to every session of this profile. None uses no preset. Applies from the next turn. |
| Max turns | `agent.max_turns` | unset (no limit) | Stop after this many tool calls for one request. Unset: no limit. Applies to new sessions. |
| Coding instructions | `agent.coding_instructions` | `''` | Your own rules the agent follows when it works on code. Applies to new sessions. |
| Shorten long chats | `compression.enabled` | see config | Summarize older messages when a chat nears the model's limit. Applies to new sessions. |
| Shorten at | `compression.threshold` | see config | How full the chat gets before older messages are summarized. A share of the model's context window: 0.5 is half full. Applies to new sessions. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Saved prompt | `custom_prompt` | `''` | A note /prompt keeps for you, which the agent never reads. |
| Bot mode rules | `agent.bot_mode_protocol` | see config | Tells the agent how to behave when relayed through a bot. Applies to new sessions. |
| Budget warning point | `agent.budget_warning_ratio` | unset (no warning) | Warns the agent when this share of its turns is used. A number between 0 and 1, such as 0.8 for 80 percent. Other values turn the warning off. Unset: no warning. Applies to new sessions. |
| Coding mode | `agent.coding_context` | see config | Whether the agent switches to coding guidance in code folders. One of: auto, focus, on, off. Applies to new sessions. |
| Retry cost limit | `agent.empty_response_guard.cost_threshold_usd` | see config | Spend in dollars above which empty replies are not retried. Must be above zero or the built-in value is used. Applies to new sessions. |
| Empty reply guard | `agent.empty_response_guard.enabled` | see config | Retries when the model sends back an empty reply. Applies to new sessions. |
| Environment note | `agent.environment_hint` | `''` | Extra text describing your machine, added to the agent's prompt. Applies to new sessions. |
| Check environment | `agent.environment_probe` | see config | Lets the agent look at installed tools before its first turn. Applies to new sessions. |
| Execution guidance | `agent.execution_guidance` | see config | Adds extra advice on acting and finishing work to the prompt. Auto turns it on for model families that need it. One of: auto, always, never. Applies to new sessions. |
| Continue after intent | `agent.intent_ack_continuation` | see config | Keeps going when the model only says what it will do. Auto turns it on only for Codex style models. One of: auto, always, never. Applies to new sessions. |
| Verify reminders | `agent.max_verify_nudges` | see config | Most times per turn the agent is told to verify its work. Zero turns the reminders off. |
| Parallel tool advice | `agent.parallel_tool_call_guidance` | see config | Tells the agent to run independent tool calls together. Applies to new sessions. |
| Run time limit | `agent.run_budget_seconds` | unset (no limit) | Longest time one run may take before the agent is told to wrap up. Unset: no limit. Applies to new sessions. |
| Stall guards | `agent.stall_guards` | see config | Stops the agent when it repeats the same calls without progress. Applies to new sessions. |
| Finish tasks advice | `agent.task_completion_guidance` | see config | Reminds the agent to finish a task before it stops. Applies to new sessions. |
| Tool use nudge | `agent.tool_use_enforcement` | see config | Pushes the model to call tools instead of only describing steps. Auto turns it on for model families that need it. One of: auto, always, never. Applies to new sessions. |
| Verify advice | `agent.verify_guidance` | see config | Adds how-to-verify advice to the reminders sent before stopping. |
| Verify before stop | `agent.verify_on_stop` | see config | Asks the agent to check its edits before it finishes. One of: auto, on, off. |
| Bot turn wait | `bot_mode.turn_wait_seconds` | see config | How long a queued bot message waits for the running turn. |
| Stop on failure | `compression.abort_on_summary_failure` | see config | Stops the turn if the summary of old messages fails. |
| Require checkpoint | `compression.checkpoint_required` | see config | Refuses to compress until a checkpoint of the chat is saved. Not allowed with the Codex app server mode. |
| Codex compaction | `compression.codex_app_server_auto` | see config | Who compacts a long Codex app server conversation. One of: native, icebath, off. |
| Raise Codex trigger | `compression.codex_gpt55_autoraise` | see config | Compresses later for the newest Codex models. |
| Codex raise notice | `compression.codex_gpt55_autoraise_notice` | see config | Shows a note when the Codex compression trigger is raised. |
| Codex compact size | `compression.codex_responses_compact_threshold` | unset (chosen automatically) | Size in tokens at which native Codex compaction starts. Only used when native Codex compaction is on. Must be above zero. Unset: chosen automatically. |
| Native Codex compaction | `compression.codex_responses_native` | see config | Lets Codex compact long conversations itself. |
| Summary idle limit | `compression.context_timeout_seconds` | see config | Longest the summary step may stay silent before it is given up. Raised automatically to fit the summary model's own timeout. |
| Summary time limit | `compression.context_total_ceiling_seconds` | see config | Longest a whole summary step may take. |
| Cleanup retry pause | `compression.hygiene_failure_cooldown_seconds` | see config | Wait before retrying a failed automatic cleanup of a long chat. |
| Cleanup message limit | `compression.hygiene_hard_message_limit` | see config | Message count that forces a cleanup of a long chat. |
| Cleanup hold time | `compression.hygiene_max_turn_hold_seconds` | see config | Longest a new message waits while a chat is being cleaned up. |
| Cleanup idle limit | `compression.hygiene_timeout_seconds` | see config | Longest an automatic cleanup may stay silent before it fails. |
| Cleanup time limit | `compression.hygiene_total_ceiling_seconds` | see config | Longest a whole automatic cleanup may take. |
| Compact when idle | `compression.idle_compact_after_seconds` | see config | Compresses a chat after it has sat idle this long. Zero turns it off. |
| Compress in place | `compression.in_place` | see config | Keeps the same session when old messages are summarised. |
| Summary attempts | `compression.max_attempts` | see config | How many times a failed summary is retried. Between 1 and 10. |
| Micro compaction | `compression.micro_compact` | see config | Trims old tool output a little at a time instead of all at once. |
| Micro compact repack | `compression.micro_compact_defrag_threshold_tokens` | see config | Size in tokens that triggers a repack of trimmed output. |
| Micro compact pace | `compression.micro_compact_every_n_turns` | see config | Trims old tool output once every this many turns. |
| Keep user messages | `compression.min_tail_user_messages` | see config | Fewest of your latest messages always kept as written. |
| Per model trigger | `compression.model_thresholds` | `{}` | Compression trigger share for chosen models, by model name. Each value is a fraction of the model's context window, such as 0.6. |
| Summarise ahead | `compression.prepare_ahead` | see config | Prepares the summary before the trigger is reached. |
| Prune minimum gain | `compression.proactive_prune_min_reclaim_tokens` | see config | Least tokens a trim must free to be worth doing. |
| Prune result size | `compression.proactive_prune_min_result_chars` | see config | Tool output shorter than this is never trimmed. |
| Early trim size | `compression.proactive_prune_tokens` | see config | Token size at which old tool output is trimmed early. Zero turns early trimming off. |
| Compression notices | `compression.progress_notices` | see config | Shows status messages while a chat is being compressed. |
| Keep first messages | `compression.protect_first_n` | see config | Number of opening messages never summarised. |
| Keep last messages | `compression.protect_last_n` | see config | Number of latest messages never summarised. |
| Recent history mode | `compression.tail_mode` | see config | How recent messages are kept when compressing. One of: lean, legacy. |
| Summary size | `compression.target_ratio` | see config | Share of the trigger size the summary is squeezed to. Held between 0.1 and 0.8. |
| Trigger token cap | `compression.threshold_tokens` | unset (share of context window) | Hard cap in tokens that triggers compression. Unset: share of context window. |
| Context engine | `context.engine` | see config | Which engine manages long conversations. Compressor is the built-in one. Plugins can add others. Applies to new sessions. |
| Context file limit | `context_file_max_chars` | unset (sized to the model) | Longest project instruction file text sent, in characters. Unset: sized to the model. |
| Context file wait | `context_file_read_timeout` | see config | Longest wait while reading a project instruction file. |
| Subagent API key | `delegation.api_key` | secret, in .env | Key used by subagents that use their own endpoint. |
| Subagent API style | `delegation.api_mode` | `''` | Request format of the subagent endpoint. Leave empty to detect it from the endpoint address. |
| Subagent backend | `delegation.backend` | see config | Which backend runs subagents. Default runs them in this app. Plugins can add others. Applies to new sessions. |
| Backend settings | `delegation.backends` | `{}` | Settings for each subagent backend, by backend name. |
| Subagent endpoint | `delegation.base_url` | `''` | Own model endpoint address for subagents. Leave empty to use the subagent provider or the main model. |
| Subagent idle limit | `delegation.child_timeout_seconds` | see config | Stops a subagent that has been inactive this long. Zero means no limit. Anything set is raised to at least 30 seconds. |
| Subagent compress at | `delegation.compression_threshold_tokens` | see config | Token size at which a subagent compresses its chat. Zero uses the normal share of the context window. Values under 16000 are ignored. |
| Subagent fallbacks | `delegation.fallback_providers` | unset (same as main agent) | Providers subagents try when their own fails. Unset: same as main agent. |
| Subagents in chats | `delegation.gateway_mode` | see config | Whether chat subagents finish within the turn or in the background. One of: auto, inline, background. |
| Separate completions | `delegation.independent_completions` | see config | Reports each background subagent on its own, not as a group. |
| Subagents keep MCP | `delegation.inherit_mcp_toolsets` | see config | Narrowed subagents still get the parent's MCP tools. |
| Parallel subagents | `delegation.max_concurrent_children` | see config | Most subagents that run at the same time. Each one uses model tokens, so high values raise cost. |
| Subagent turn limit | `delegation.max_iterations` | see config | Most tool turns one subagent may take. |
| Subagent depth | `delegation.max_spawn_depth` | see config | How many levels deep subagents may start more subagents. Each level multiplies cost. |
| Subagent summary size | `delegation.max_summary_chars` | see config | Longest subagent report returned to the parent. Longer reports are trimmed and saved to disk. |
| Subagent model | `delegation.model` | `''` | Model subagents use instead of the main one. |
| One-shot subagent limit | `delegation.oneshot_max_children` | see config | Most subagents one single-run session may start. Zero means no limit. |
| Subagent orchestrators | `delegation.orchestrator_enabled` | see config | Lets subagents coordinate subagents of their own. |
| Subagent worktrees | `delegation.worktree_isolation` | see config | Give each subagent its own git worktree off your current commit. Applies from the next turn. |
| Subagent provider | `delegation.provider` | `''` | Provider subagents use instead of the main one. |
| Subagent effort | `delegation.reasoning_effort` | unset (the main agent's level) | How long subagents think before answering. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the main agent's level. |
| Subagent request fields | `delegation.request_overrides` | `{}` | Extra request fields sent with subagent model calls. |
| Auto-approve subagents | `delegation.subagent_auto_approve` | see config | Lets subagents run risky commands without asking. When off, risky commands from subagents are refused. |
| Subagent process news | `delegation.surface_child_process_notifications` | see config | Shows background process updates started by subagents. |
| Goal turn limit | `goals.max_turns` | see config | Most turns an active goal may use. |
| Auto split tasks | `kanban.auto_decompose` | see config | Splits new board tasks into smaller ones automatically. |
| Splits per pass | `kanban.auto_decompose_per_tick` | see config | Most tasks split in one check of the board. |
| Auto start subtasks | `kanban.auto_promote_children` | see config | Moves new subtasks forward as soon as they are made. |
| Follow new tasks | `kanban.auto_subscribe_on_create` | see config | Follows a task for you when you create it. |
| Default assignee | `kanban.default_assignee` | `''` | Profile that takes tasks nobody was assigned. |
| Board health limits | `kanban.diagnostics` | unset (the built-in thresholds) | Custom limits for board health warnings. Leave unset for the built-in limits. Unset: the built-in thresholds. |
| Board in service | `kanban.dispatch_in_gateway` | see config | Starts board tasks from the background service. Restart the background service to apply. |
| Board check interval | `kanban.dispatch_interval_seconds` | see config | How often the board looks for tasks to start. At least one second. |
| Allowed profiles | `kanban.dispatch_profiles` | unset (any profile) | Profiles allowed to pick up board tasks. Unset: any profile. |
| Stale task timeout | `kanban.dispatch_stale_timeout_seconds` | see config | Marks a running task stale after this long without progress. Zero turns stale detection off. |
| Keep done follows | `kanban.done_sub_retention_days` | see config | How long follows on finished tasks are kept. |
| Task failure limit | `kanban.failure_limit` | see config | Failures after which a task stops being retried. |
| Tasks at once | `kanban.max_in_progress` | unset (sized to free memory) | Most board tasks running at the same time. Must be at least 1. Unset: sized to free memory. |
| Tasks per profile | `kanban.max_in_progress_per_profile` | unset (no limit) | Most running board tasks per profile. Unset: no limit. |
| Starts per pass | `kanban.max_spawn` | unset (no limit) | Most tasks started in one check of the board. Unset: no limit. |
| Board alerts | `kanban.notify_in_gateway` | see config | Sends board updates from the background service. Restart the background service to apply. |
| Planner profile | `kanban.orchestrator_profile` | `''` | Profile that plans and splits board tasks. |
| Recover lost tasks | `kanban.reconcile_orphans` | see config | Recovers running tasks whose worker has vanished. |
| Auto start reviews | `kanban.review_dispatch` | see config | Starts review tasks automatically. |
| Task log copies | `kanban.worker_log_backup_count` | see config | Old task log files kept after a log rolls over. |
| Task log size | `kanban.worker_log_rotate_bytes` | see config | Size at which a task log rolls over. |
| Loop run limit | `loops.max_ticks` | see config | Most runs one loop may make. Zero means no limit. |
| Loop shortest gap | `loops.min_interval_seconds` | see config | Shortest time allowed between loop runs. |
| Self-paced longest gap | `loops.self_paced_ceiling_seconds` | see config | Longest wait a self-paced loop may choose. |
| Self-paced shortest gap | `loops.self_paced_floor_seconds` | see config | Shortest wait a self-paced loop may choose. |
| Active mix | `moa.active_preset` | `''` | Mixture preset currently in use. |
| Default mix | `moa.default_preset` | see config | Mixture preset used when none is chosen. |
| Mixture presets | `moa.presets` | see config | Named groups of models that answer together, with one combining model. |
| Mixture privacy | `moa.privacy_filter` | `''` | Hides private details sent to the mixture's models. One of: , display, full. |
| Save mixture traces | `moa.save_traces` | see config | Saves a record of each mixture run to disk. |
| Trace folder | `moa.trace_dir` | `''` | Where mixture records are saved. Leave empty to use a folder in your profile. |
| Personalities | `personalities` | `{}` | Custom personalities you can switch to, by name. |
| Prefill messages file | `prefill_messages_file` | `''` | JSON file of example messages added to every conversation. A relative path starts from your profile folder. |
| Timezone | `timezone` | `''` | Time zone for dates, times and schedules, such as Europe/Berlin. Leave empty to use this computer's time zone. |
| Stop: same failure | `tool_loop_guardrails.hard_stop_after.exact_failure` | see config | Stops after the same failing call repeats this many times. |
| Stop: no progress | `tool_loop_guardrails.hard_stop_after.idempotent_no_progress` | see config | Stops after a read-only call repeats with no new result. |
| Stop: tool failures | `tool_loop_guardrails.hard_stop_after.same_tool_failure` | see config | Stops after one tool keeps failing this many times. |
| Stop looping tools | `tool_loop_guardrails.hard_stop_enabled` | see config | Stops a turn that is stuck in a tool loop. |
| Subagents per turn | `tool_loop_guardrails.loop_caps.max_subagents` | see config | Most subagents one turn may start. Zero means no limit. |
| Searches per turn | `tool_loop_guardrails.loop_caps.max_web_searches` | see config | Most web searches one turn may make. Zero means no limit. |
| Stop loops unattended | `tool_loop_guardrails.non_interactive_hard_stop_enabled` | see config | Always stops tool loops in chats and scheduled runs. |
| Warn: same failure | `tool_loop_guardrails.warn_after.exact_failure` | see config | Warns the agent after the same failing call repeats this often. |
| Warn: no progress | `tool_loop_guardrails.warn_after.idempotent_no_progress` | see config | Warns after a read-only call repeats with no new result. |
| Warn: tool failures | `tool_loop_guardrails.warn_after.same_tool_failure` | see config | Warns the agent after one tool keeps failing this often. |
| Loop warnings | `tool_loop_guardrails.warnings_enabled` | see config | Warns the agent when it seems stuck in a tool loop. |

## Tools

Which tools the agent may use.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Tool groups | `platform_toolsets` | `{}` | Which groups of tools the agent may call, per app. The terminal UI uses the cli entry and each chat app its own. /tools changes the current one. Applies to new sessions. |
| Web search | `web.backend` | `''` | Which service answers the agent's web searches. Automatic picks the first service with a key, else a free one. One of: , exa, parallel, keenable, firecrawl, tavily, perplexity, searxng, brave-free, ddgs, xai. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Disabled toolsets | `agent.disabled_toolsets` | `[]` | Tool groups to switch off for every chat. Applies on top of Tool groups. |
| Browser service | `browser.cloud_provider` | see config | Where web pages open: local, or a cloud browser service. |
| Auto start desktop | `bot_desktop.auto_start` | see config | Start a hidden desktop on its own when a tool needs a screen. Only applies on a Linux machine with no display and the needed packages installed. |
| Desktop size | `bot_desktop.geometry` | see config | Screen size of the agent's own desktop, as width x height. |
| Desktop idle stop | `bot_desktop.idle_stop_minutes` | see config | Stop the agent's desktop after this long without use. Zero or less keeps it running. |
| Min free memory | `bot_desktop.min_free_memory_mb` | see config | Refuse to start a desktop when free memory is below this. Zero turns the check off. |
| Desktop placement | `bot_desktop.placement` | see config | Where the agent's desktop runs. One of: auto, terminal, gateway. |
| Allow private addresses | `browser.allow_private_urls` | see config | Let the browser open local and private network addresses. |
| Local for private | `browser.auto_local_for_private_urls` | see config | Use the local browser for private addresses instead of a cloud one. |
| Camofox options | `browser.camofox` | `{}` | Options for the Camofox browser backend. A block of sub-settings such as managed_persistence, cookies_dir, timeout and temperature. |
| Browser attach address | `browser.cdp_url` | `''` | Attach to an already running browser at this address. The BROWSER_CDP_URL environment variable and a live /browser connect both win over this. |
| Extra browser flags | `browser.chromium_args` | `[]` | Extra command line flags added to every Chromium launch. |
| Browser start timeout | `browser.command_timeout` | see config | How long to wait for the browser to start and answer. Never less than 5 seconds. |
| Page dialogs | `browser.dialog_policy` | see config | How the browser handles alert, confirm and prompt pop-ups. One of: must_respond, auto_dismiss, auto_accept. |
| Dialog wait | `browser.dialog_timeout_s` | see config | How long a pop-up waits for an answer before it is dealt with. Zero or less uses the default. |
| Browser engine | `browser.engine` | see config | Which local browser engine the tools drive. One of: auto, lightpanda. |
| Browser program | `browser.executable_path` | `''` | Use this browser program instead of the bundled one. |
| Show browser window | `browser.headed` | see config | Show the browser window instead of running it hidden. |
| Browser idle close | `browser.inactivity_timeout` | see config | Close an unused browser session after this long. Never less than 30 seconds. Falls back to the BROWSER_INACTIVITY_TIMEOUT environment variable only when this is unset. |
| Close real browser | `browser.real_profile_autoclose` | see config | Close your own browser by itself when the agent needs its profile. Only matters when using your real browser profile is allowed. |
| Real profile name | `browser.real_profile_pin` | `''` | Folder name of the browser profile to borrow, such as Profile 2. Empty uses the profile you used last. A name that does not exist stops the borrow. |
| Use real profile | `browser.use_real_profile` | see config | Allow the agent to use a copy of your own browser profile and logins. |
| Browser profile folder | `browser.user_data_dir` | `''` | Keep one browser profile in this folder for every session. A relative path is placed under the Icebath home folder. |
| Kernel idle close | `code_execution.kernel_idle_timeout` | see config | Close an idle code session after this long. Never less than 1 second. |
| Max code sessions | `code_execution.max_session_kernels` | see config | How many code sessions can stay open at once. |
| Code run folder | `code_execution.mode` | see config | Where scripts run: your working folder or a scratch one. One of: project, strict. |
| Allow unsigned driver | `computer_use.allow_unsigned_driver` | see config | Accept a driver app that lacks the expected publisher signature. Only for locally built drivers on macOS. |
| Auto start driver | `computer_use.autostart` | see config | Start the computer-use driver on its own when first needed. |
| Max screen elements | `computer_use.ax_max_elements` | see config | Most on-screen items listed per accessibility snapshot. Zero means no limit. |
| Computer use backend | `computer_use.backend` | see config | Which computer-use provider controls the screen. Names a built-in or plugin provider. Unknown names stop the tool with a list of the available ones. |
| Capability file | `computer_use.capability_manifest` | `''` | Path to a file limiting what the driver may do. Used only outside the standard permission mode. |
| Follow-up capture | `computer_use.capture_after_mode` | see config | What each action sends back after it runs. One of: som, vision, ax. |
| Driver telemetry | `computer_use.cua_telemetry` | see config | Let the computer-use driver send usage data. |
| Max screenshot edge | `computer_use.max_image_dimension` | see config | Longest side of a screenshot sent to the model, in pixels. Zero or less turns the cap off. |
| Native Wayland | `computer_use.native_wayland` | see config | Use Wayland directly instead of the X11 compatibility layer. |
| Hide overlay | `computer_use.no_overlay` | unset (decided automatically) | Turn the on-screen cursor overlay off. Unset: decided automatically. |
| Computer use access | `computer_use.permission_mode` | see config | How much of the screen the agent may control. Any value other than bounded is treated as standard. One of: standard, bounded. |
| Archive after | `curator.archive_after_days` | see config | Move a skill to the archive after this many idle days. Counted in days, at least 1. |
| Delete archive after | `curator.archive_ttl_days` | see config | Delete archived skills older than this many days. Zero keeps archived skills forever. |
| Back up first | `curator.backup.enabled` | see config | Save a snapshot of your skills before each curator run. |
| Snapshots kept | `curator.backup.keep` | see config | How many skill snapshots to keep. At least 1. |
| Merge skills | `curator.consolidate` | see config | Let the curator merge related skills with a model pass. Off means each run only archives idle skills. |
| Skill curator | `curator.enabled` | see config | Automatically tidy up unused skills in the background. |
| Curator interval | `curator.interval_hours` | see config | How long between automatic curator runs. At least 1 hour. |
| Curator idle wait | `curator.min_idle_hours` | see config | How long you must be idle before an automatic run starts. |
| Prune built-in skills | `curator.prune_builtins` | see config | Let the curator archive unused built-in skills too. |
| Stale after | `curator.stale_after_days` | see config | Mark a skill stale after this many idle days. Counted in days, at least 1. |
| File read limit | `file_read_max_chars` | see config | Most characters returned from one file read. Longer files are cut at a whole line. |
| Shell hooks | `hooks` | `{}` | Commands to run on agent events, grouped by event name. Each event holds a list of hook definitions. Unknown event names are ignored with a warning. |
| MCP connect limit | `mcp.discovery_concurrency` | see config | How many MCP servers connect at the same time during discovery. Zero means no limit. |
| MCP startup wait | `mcp_discovery_timeout` | see config | How long a new turn waits for MCP servers to be found. Zero or less uses the default. |
| MCP one-shot wait | `mcp_single_query_discovery_timeout` | see config | How long a one-shot query waits for MCP servers to be found. Zero or less uses the default. |
| Auto update plugins | `plugins.auto_apply` | see config | Install plugin updates without asking. |
| Plugin update check | `plugins.auto_update_check_hours` | see config | How often to check plugins for updates. Zero turns the check off. |
| Plugin download limit | `plugins.clone_timeout_seconds` | see config | Longest a plugin download may take. Between 1 second and 1 hour. |
| Plugin hook limit | `plugins.hook_callback_timeout` | see config | Longest a plugin hook may take before it is cut off. Capped at 10 minutes. |
| Plugin launcher | `plugins.host.launcher` | `[]` | Command placed before the plugin host process, such as a sandbox. |
| Plugin load limit | `plugins.load_timeout_seconds` | see config | Longest a single plugin may take to load. Zero loads plugins inline with no limit. Capped at 10 minutes. |
| Quick commands | `quick_commands` | `{}` | Your own slash commands, each running a shell command or alias. A mapping of command name to its definition. |
| Preloaded skills | `skills.auto_load` | `[]` | Skills loaded into every new session. |
| Skill settings | `skills.config` | `{}` | Values the skills you installed ask for, by name. |
| New skill folder | `skills.create_dir` | `''` | Folder where new skills are created. Empty uses the normal skills folder. |
| Disabled skills | `skills.disabled` | `[]` | Skills the agent cannot use. |
| Extra skill folders | `skills.external_dirs` | `[]` | More folders to look for skills in. |
| Scan agent skills | `skills.guard_agent_created` | see config | Security-scan skills the agent writes and block dangerous ones. |
| Skill shell snippets | `skills.inline_shell` | see config | Run shell snippets embedded in skill text. Never applied to skills from the community hub. |
| Snippet time limit | `skills.inline_shell_timeout` | see config | Longest an embedded shell snippet may run. |
| Skill change log | `skills.ledger` | see config | Keep a log of every skill change by you, the agent or the curator. |
| Change log size | `skills.ledger_max_bytes` | see config | Size at which the skill change log is trimmed. |
| Per-app disabled | `skills.platform_disabled` | `{}` | Skills turned off for a single app, by app name. A mapping of app name to a list of skill names. |
| Project skills | `skills.project_discovery` | see config | Find skills inside the current project folder. Project skills load only when the project is trusted. |
| Skill variables | `skills.template_vars` | see config | Fill in placeholders such as the skill folder in skill text. |
| Install safety check | `skills.tier1_advisory` | see config | Show a quick safety scan of a skill when you install it. |
| Trusted projects | `skills.trusted_project_dirs` | `[]` | Project folders whose own skills may be loaded. |
| Approve skill writes | `skills.write_approval` | see config | Ask before the agent saves or changes a skill. |
| Tool output size | `tool_output.max_bytes` | see config | Most bytes of a tool result kept before it is cut. |
| Tool line length | `tool_output.max_line_length` | see config | Most characters kept per line of a tool result. |
| Tool output lines | `tool_output.max_lines` | see config | Most lines of a tool result kept before it is cut. |
| Deferred tools | `tools.tool_search.defer` | see config | Tools hidden until the agent searches for them, to save context. An empty list keeps every tool loaded up front. |
| Tool search | `tools.tool_search.enabled` | see config | Whether rarely used tools are found by search instead of listed. One of: auto, on, off. |
| Tool listing | `tools.tool_search.listing` | see config | Whether hidden tools are summarized in the prompt. One of: auto, on, off. |
| Listing size | `tools.tool_search.listing_max_tokens` | see config | Most tokens spent summarizing hidden tools. Between 200 and 60000. |
| Max search results | `tools.tool_search.max_search_limit` | see config | Most tools one search may return. |
| Default search results | `tools.tool_search.search_default_limit` | see config | How many tools a search returns when none is asked for. Never above the maximum. |
| Listing context share | `tools.tool_search.threshold_pct` | see config | Share of the context window the tool summary may use. |
| Toolsets | `toolsets` | `- icebath-cli` | Only used to turn on the Kanban tools when listed. Tool groups are normally chosen per platform, not here. |
| Web cache | `web.cache_enabled` | see config | Reuse recent web search and page results. |
| Uncached sites | `web.cache_exempt_hosts` | `[]` | Sites whose pages are always fetched fresh. Matches a site and its subdomains. |
| Web cache time | `web.cache_ttl_minutes` | see config | How long a cached web result is reused. Between 1 minute and 1 day. |
| Page reader provider | `web.extract_backend` | `''` | Provider used to read web pages. Empty follows the main web provider. |
| Page text limit | `web.extract_char_limit` | see config | Most characters kept from one web page. Between 2000 and 500000. |
| Free web providers | `web.keyless_fallback` | see config | Use free providers when no web search key is set. |
| Free provider retry | `web.keyless_rescue` | see config | Retry with a free provider when the chosen one fails. Needs free web providers to be on. |
| Provider plan | `web.provider_tier` | `{}` | Per provider: use its free plan, a paid key, or decide automatically. |
| Web search provider | `web.search_backend` | `''` | Provider used for web search. Empty follows the main web provider. |
| X search model | `x_search.model` | see config | Model that runs X searches. |
| X search effort | `x_search.reasoning_effort` | unset (the model's default) | How hard the model thinks while searching X. One of: low, medium, high, xhigh. Unset: the model's default. |
| X search retries | `x_search.retries` | see config | How many times a failed X search is retried. |
| X search timeout | `x_search.timeout_seconds` | see config | Longest to wait for an X search. Never less than 30 seconds. |
| MCP servers | `mcp_servers` | `{}` | Tool servers the agent connects to, by name. Applies to new sessions. |
| Offered toolsets | `known_builtin_toolsets` | `{}` | Built-in toolsets each app has seen; new ones turn on. |
| Offered plugin tools | `known_plugin_toolsets` | `{}` | Plugin toolsets each app has seen; new ones turn on. |

## Terminal

Where the agent's shell commands run.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Where commands run | `terminal.backend` | see config | The machine or sandbox where the agent's shell commands run. One of: local, docker, ssh, modal, daytona, vercel_sandbox. Applies to new sessions. |
| Working folder | `terminal.cwd` | see config | Folder the agent's shell commands start in. |
| Container image | `terminal.docker_image` | see config | Image the Docker sandbox starts from. Applies to new sessions. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Auto-prune snapshots | `checkpoints.auto_prune` | see config | Delete old file snapshots automatically at startup. |
| File checkpoints | `checkpoints.enabled` | see config | Save file snapshots so edits can be rewound. Applies to new sessions. |
| Max file size | `checkpoints.max_file_size_mb` | see config | Largest single file a snapshot will include. Applies to new sessions. |
| Snapshots kept | `checkpoints.max_snapshots` | see config | How many snapshots are kept per project. Applies to new sessions. |
| Snapshot disk limit | `checkpoints.max_total_size_mb` | see config | Total disk space snapshots may use before pruning. Applies to new sessions. |
| Prune interval | `checkpoints.min_interval_hours` | see config | Fewest hours between automatic snapshot cleanups. |
| Snapshot retention | `checkpoints.retention_days` | see config | Days an old snapshot is kept before cleanup removes it. |
| Allow key fallback | `proxy.allow_env_fallback` | see config | Use keys from the environment if the secret store is off. Only matters when the credential source is Bitwarden. |
| Auto-install proxy | `proxy.auto_install` | see config | Download the egress proxy program when it is missing. |
| Proxy key source | `proxy.credential_source` | see config | Where the egress proxy gets the real API keys it injects. One of: env, bitwarden. |
| Egress proxy | `proxy.enabled` | see config | Route sandbox network traffic through the key-hiding proxy. Run the egress setup command first. Applies to Docker sandboxes. Applies to new sessions. |
| Require proxy | `proxy.enforce_on_docker` | see config | Refuse to start a Docker sandbox if the proxy is not ready. When off, the sandbox starts without the proxy and warns. Applies to new sessions. |
| Extra allowed hosts | `proxy.extra_allowed_hosts` | `[]` | Extra sites the egress proxy lets sandboxes reach. Written into the proxy when egress setup runs. |
| Proxy port | `proxy.tunnel_port` | see config | Local port the egress proxy listens on. Written into the proxy when egress setup runs. |
| Blocked networks | `proxy.upstream_deny_cidrs` | unset (built-in private ranges) | Network ranges the egress proxy refuses to connect to. Leave unset for the built-in private network list. An empty list blocks nothing. Unset: built-in private ranges. |
| Load shell profile | `terminal.auto_source_bashrc` | see config | Load your usual shell startup files before running commands. Ignored when a shell init file list is set. |
| Client variables | `terminal.client_env_passthrough` | `[]` | Extra variable names copied from the connecting terminal. Names only. Secrets are filtered out. Applies to new sessions. |
| Sandbox CPUs | `terminal.container_cpu` | see config | CPU cores given to a container or cloud sandbox. Applies to new sessions. |
| Sandbox disk | `terminal.container_disk` | see config | Disk space given to a container or cloud sandbox. Applies to new sessions. |
| Sandbox memory | `terminal.container_memory` | see config | Memory given to a container or cloud sandbox. Applies to new sessions. |
| Keep sandbox files | `terminal.container_persistent` | see config | Keep a sandbox's files between sessions. Applies to new sessions. |
| Credential files | `terminal.credential_files` | `[]` | Files under the config folder copied into remote sandboxes. Paths are relative to the icebath home folder. Applies to new sessions. |
| Stop grace period | `terminal.daemon_term_grace_seconds` | see config | Wait before force-killing a background process that was asked to stop. Zero kills it immediately. |
| Daytona image | `terminal.daytona_image` | see config | Image Daytona sandboxes start from. Applies to new sessions. |
| Backend failure mode | `terminal.degraded_mode` | see config | What happens when the place commands run cannot be reached. One of: warn, fail. Applies to new sessions. |
| Docker variables | `terminal.docker_env` | `{}` | Variables set inside Docker sandboxes, as name and value. Applies to new sessions. |
| Docker extra flags | `terminal.docker_extra_args` | `[]` | Extra flags added to the Docker run command. Applies to new sessions. |
| Docker forwarded variables | `terminal.docker_forward_env` | `[]` | Host variables passed into Docker sandboxes. Names only. The values come from your environment. Applies to new sessions. |
| Mount working folder | `terminal.docker_mount_cwd_to_workspace` | see config | Mount your current folder into the Docker sandbox. Applies to new sessions. |
| Docker network | `terminal.docker_network` | see config | Let Docker sandboxes reach the network. Applies to new sessions. |
| Clean stray containers | `terminal.docker_orphan_reaper` | unset (on) | Remove leftover Docker sandboxes from earlier runs. Unset: on. Applies to new sessions. |
| Reuse Docker sandbox | `terminal.docker_persist_across_processes` | unset (on) | Keep a Docker sandbox alive so later runs can reuse it. Unset: on. Applies to new sessions. |
| Run as me | `terminal.docker_run_as_host_user` | see config | Run Docker commands as your own user, not root. Applies to new sessions. |
| Shared sandbox name | `terminal.docker_shared_container_key` | `''` | Name that lets sessions share one Docker sandbox. Empty means each session gets its own. Applies to new sessions. |
| Docker shared memory | `terminal.docker_shm_size` | see config | Shared memory size for Docker sandboxes, such as 1g. Applies to new sessions. |
| Snap Docker mode | `terminal.docker_snap_compat` | see config | Work around Docker installed as a snap package. Applies to new sessions. |
| Docker volumes | `terminal.docker_volumes` | `[]` | Folders mounted into Docker sandboxes, as host:container. Applies to new sessions. |
| Passthrough env names | `terminal.env_passthrough` | `[]` | Extra variable names commands may see from your environment. Provider keys are refused here. |
| Home folder mode | `terminal.home_mode` | see config | Which home folder commands see. One of: auto, profile, real. Applies to new sessions. |
| Modal image | `terminal.modal_image` | see config | Image Modal sandboxes start from. Applies to new sessions. |
| One-shot wait limit | `terminal.oneshot_completion_wait_seconds` | see config | Longest wait for a one-shot background command to finish. |
| Persistent SSH shell | `terminal.persistent_shell` | see config | Keep one shell open between remote commands over SSH. Other backends have their own persistence settings. Applies to new sessions. |
| Notify on finish | `terminal.process_wait_auto_notify` | see config | Tell the agent when a waited-on background process ends. |
| Sandbox folder | `terminal.sandbox_dir` | unset (the sandboxes folder in the icebath home) | Host folder where sandbox files are stored. Unset uses the sandboxes folder in the icebath home. Unset: the sandboxes folder in the icebath home. Applies to new sessions. |
| Shell init files | `terminal.shell_init_files` | `[]` | Files sourced before commands run, instead of the usual ones. Empty falls back to the profile and bashrc files. |
| Sync-back size cap | `terminal.sync_back_max_bytes` | see config | Largest archive copied back from a remote sandbox. |
| Temp folder | `terminal.temp_dir` | `''` | Folder commands use for temporary files. Empty uses the normal system temp folder. Applies to new sessions. |
| Command timeout | `terminal.timeout` | see config | Longest a foreground command may run before it is stopped. Applies to new sessions. |
| Vercel image | `terminal.vercel_image` | see config | Image Vercel sandboxes start from. Applies to new sessions. |
| Vercel runtime | `terminal.vercel_runtime` | `''` | Language runtime for Vercel sandboxes. Empty uses the image setting instead. Applies to new sessions. |
| Sandbox idle limit | `terminal.lifetime_seconds` | see config | Idle time after which an unused sandbox is shut down. Applies to new sessions. |
| Archive dirty trees | `worktrees.archive_dirty` | see config | Save a patch of unsaved work before removing a worktree. |
| SSH host | `terminal.ssh_host` | `''` | Machine the agent's commands run on when they run over SSH. Applies to new sessions. |
| Archive file limit | `worktrees.archive_max_file_mb` | see config | Largest file included in a worktree archive. |
| SSH user | `terminal.ssh_user` | `''` | Account to log in as over SSH. Applies to new sessions. |
| Archive retention | `worktrees.archive_retention_days` | see config | Days a removed worktree's archive is kept. |
| SSH port | `terminal.ssh_port` | see config | Port to connect to over SSH. Applies to new sessions. |
| Install cache retention | `worktrees.install_state_retention_days` | see config | Days unused dependency install data is kept. Zero or less turns cleanup off. |
| SSH key file | `terminal.ssh_key` | `''` | Private key file used to log in over SSH. Empty uses your SSH agent and default keys. Applies to new sessions. |
| Worktree disk limit | `worktrees.max_total_gb` | see config | Total disk space worktrees may use before cleanup. Zero means no limit. |
| Apptainer image | `terminal.singularity_image` | unset (the default sandbox image) | Image the Apptainer sandbox starts from. Unset: the default sandbox image. Applies to new sessions. |
| Persistent local shell | `terminal.local_persistent` | see config | Keep one shell open between commands run on this machine. Off starts a fresh shell for every command. Applies to new sessions. |
| Own work tree | `worktree` | see config | Start each terminal session in its own git work tree. Applies to new sessions. |
| Fetch before branching | `worktree_sync` | see config | Fetch the upstream branch before making a new work tree. Off branches from your local HEAD. Applies to new sessions. |

## Safety

What the agent asks first and what it keeps private.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Approvals | `approvals.mode` | see config | Whether the agent asks before running risky commands. /yolo skips asking for one session without changing this. One of: manual, smart, off. |
| Allowed commands | `command_allowlist` | `[]` | Commands you chose to always allow, which run without asking. |
| Confirm destructive | `approvals.destructive_slash_confirm` | see config | Ask before slash commands that delete or reset things. |
| Hide personal info | `privacy.redact_pii` | see config | Hide who wrote a chat app message and where from the model. Applies from the next turn. |
| Hide secrets | `security.redact_secrets` | see config | Mask API keys and tokens in replies, session files and logs. Restart the background service to apply. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Scheduled job approvals | `approvals.cron_mode` | see config | How Approvals treat risky commands in scheduled jobs. Approve runs risky commands with nobody watching. One of: deny, approve. |
| Denial limit | `approvals.denial_breaker_threshold` | see config | Stop a session after this many Approvals denials in a row. Zero or less turns the limit off. |
| Always-deny patterns | `approvals.deny` | `[]` | Command patterns Approvals always block, even in yolo mode. Matched as globs against the command text. |
| Discord prompt wait | `approvals.discord_prompt_timeout` | see config | How long Discord approval buttons wait for an answer. |
| Confirm tool reload | `approvals.mcp_reload_confirm` | see config | Ask before reconnecting tool servers from a chat command. |
| One-shot approvals | `approvals.single_query_mode` | see config | How Approvals treat risky commands in one-shot runs (-q). Approve runs risky commands with nobody watching. One of: deny, approve. |
| Smart approval rules | `approvals.smart_policy` | `''` | Extra rules the approval reviewer model follows. Free text added to the reviewer's instructions. |
| Approval wait | `approvals.timeout` | see config | How long an approval prompt waits before it is denied. |
| Unattended approvals | `approvals.unattended_mode` | see config | How Approvals treat risky commands on chat platforms. Approve runs risky commands with nobody watching. One of: deny, approve. |
| Adopt other logins | `auth.adopt_external_logins` | see config | Reuse sign-ins already saved by other command-line tools. |
| Codex login style | `auth.codex_login_flow` | see config | How the Codex sign-in is done. One of: device_code, browser. |
| Auto-accept hooks | `hooks_auto_accept` | see config | Approve shell hooks without asking first. Hooks run commands on your machine, so only enable this for trusted configs. |
| Bitwarden token variable | `secrets.bitwarden.access_token_env` | see config | Environment variable holding the Bitwarden access token. |
| Auto-install Bitwarden | `secrets.bitwarden.auto_install` | see config | Download the Bitwarden command-line tool when missing. |
| Bitwarden cache time | `secrets.bitwarden.cache_ttl_seconds` | see config | How long fetched Bitwarden secrets are reused. Zero fetches them every time. |
| Bitwarden secrets | `secrets.bitwarden.enabled` | see config | Load secrets from Bitwarden Secrets Manager at startup. |
| Offline Bitwarden cache | `secrets.bitwarden.encrypted_cache.enabled` | see config | Keep an encrypted copy to use when Bitwarden is unreachable. |
| Offline cache age | `secrets.bitwarden.encrypted_cache.max_stale_seconds` | see config | Oldest offline Bitwarden copy that may still be used. Zero means no age limit. |
| Bitwarden wins | `secrets.bitwarden.override_existing` | see config | Bitwarden values replace matching local environment values. |
| Bitwarden project | `secrets.bitwarden.project_id` | `''` | Bitwarden project to load secrets from. Empty loads everything the token can see. |
| Bitwarden server | `secrets.bitwarden.server_url` | `''` | Address of your own Bitwarden server. Empty uses the Bitwarden cloud. |
| 1Password account | `secrets.onepassword.account` | `''` | 1Password account to use. Empty uses the default account. |
| 1Password tool path | `secrets.onepassword.binary_path` | `''` | Full path of the 1Password command-line tool. Empty looks it up on the search path. |
| 1Password cache time | `secrets.onepassword.cache_ttl_seconds` | see config | How long fetched 1Password secrets are reused. Zero fetches them every time. |
| 1Password secrets | `secrets.onepassword.enabled` | see config | Load secrets from 1Password at startup. |
| 1Password references | `secrets.onepassword.env` | `{}` | Variables to load, as name and op:// reference. |
| 1Password wins | `secrets.onepassword.override_existing` | see config | 1Password values replace matching local environment values. |
| 1Password token variable | `secrets.onepassword.service_account_token_env` | see config | Environment variable holding the service account token. |
| Keep local values | `secrets.preserve_existing` | `[]` | Variable names no secret store may overwrite. |
| Profile name aliases | `secrets.profile_alias` | see config | Also expose secrets under profile-prefixed variable names. |
| Dismissed advisories | `security.acked_advisories` | `[]` | Security notices you have already acknowledged. |
| Allow training plans | `security.allow_data_training_tiers_noninteractive` | see config | Let non-interactive runs go ahead on data-training plans. Otherwise such runs stop and ask for acknowledgement. |
| Allow on-demand installs | `security.allow_lazy_installs` | see config | Let tools install missing add-ons the first time they are used. |
| Allow private URLs | `security.allow_private_urls` | see config | Let web tools open local and private network addresses. |
| Approval channel | `security.approval.transport` | see config | Which channel delivers Approvals prompts. Built-in uses the normal prompt. A plugin name sends prompts through that plugin. |
| Approval fallback | `security.approval.transport_fallback` | see config | What happens when the chosen approval channel fails. One of: deny, builtin. |
| Fake IP ranges | `security.fake_ip_ranges` | `[]` | Address ranges a local DNS proxy hands out for real sites. Web tools check the real address instead of these. |
| Extra protected files | `security.protected_instruction_extra_patterns` | `[]` | Extra file names the agent needs approval to edit. |
| Protect instruction files | `security.protected_instruction_files` | see config | Ask before the agent edits project instruction files. |
| Blocked sites | `security.website_blocklist.domains` | `[]` | Websites web tools refuse to open. |
| Website blocklist | `security.website_blocklist.enabled` | see config | Block web tools from the sites on the list. |
| Blocklist files | `security.website_blocklist.shared_files` | `[]` | Files of blocked sites to load as well. |
| Bitwarden tool path | `vault.bitwarden.binary_path` | `''` | Full path of the Bitwarden command-line tool. Empty looks it up on the search path. |
| Bitwarden logins | `vault.bitwarden.enabled` | see config | Offer Bitwarden as a source of website logins. |
| Vault key storage | `vault.key_storage` | see config | Where a new vault's encryption key is kept. One of: file, keychain. |
| 1Password account | `vault.onepassword.account` | `''` | 1Password account to use for website logins. Empty uses the default account. |
| 1Password tool path | `vault.onepassword.binary_path` | `''` | Full path of the 1Password command-line tool. Empty looks it up on the search path. |
| 1Password logins | `vault.onepassword.enabled` | see config | Offer 1Password as a source of website logins. |
| 1Password token variable | `vault.onepassword.service_account_token_env` | see config | Environment variable holding the service account token. |

## Voice

Talking to the agent and hearing its replies.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Voice input | `voice.input_enabled` | see config | Talk instead of typing: a key records you and sends the words. |
| Speak replies | `voice.speak_replies` | see config | Read the assistant's replies aloud, or send voice messages in chats. The terminal UI speaks while voice input is on. A chat keeps its own choice once /voice is used there. |
| Record key | `voice.record_key` | `''` | Extra key that starts and stops a recording (default: none). The leader key followed by R always records; a binding in your keys file wins over this. |
| Send or draft | `voice.submit_mode` | see config | Send what you said at once, or leave it in the input box to edit. One of: direct, draft. |
| Speech voice | `tts.provider` | see config | Which service speaks for icebath. Each service's voice is under advanced. One of: edge, elevenlabs, openai, minimax, xai, mistral, gemini, deepinfra, neutts, kittentts, piper. |
| Speech recognition | `stt.enabled` | see config | Turn your recordings and voice messages into text. |
| Recognition service | `stt.provider` | see config | Which service turns your speech into text. One of: local, local_command, groq, openai, mistral, xai, elevenlabs, deepinfra. |
| Wake word | `wake_word.enabled` | see config | Say the wake phrase to start recording without the keyboard. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Show transcripts | `stt.echo_transcripts` | see config | Post the text of your voice messages back into the chat. Restart the background service to apply. |
| Tag helper key | `auxiliary.tts_audio_tags.api_key` | secret, in .env | Key for the model that adds tone tags to speech text. |
| Tag helper endpoint | `auxiliary.tts_audio_tags.base_url` | `''` | Custom endpoint for the model that adds tone tags to speech. |
| Tag helper fields | `auxiliary.tts_audio_tags.extra_body` | `{}` | Extra request fields sent to the tone tagging model. |
| Tag helper model | `auxiliary.tts_audio_tags.model` | `''` | Model that adds tone tags to speech text. Empty picks one for you. |
| Tag helper provider | `auxiliary.tts_audio_tags.provider` | see config | Service for the model that adds tone tags. Auto picks one for you. |
| Tag helper effort | `auxiliary.tts_audio_tags.reasoning_effort` | unset (the model's default) | How long the tone tagging model thinks. One of: none, minimal, low, medium, high, xhigh, max, ultra. Unset: the model's default. |
| Tag helper timeout | `auxiliary.tts_audio_tags.timeout` | see config | How long to wait for the tone tagging model before giving up. |
| Voice chat key | `auxiliary.voice_chat.api_key` | secret, in .env | Key for the model that answers in voice conversations. |
| Voice chat endpoint | `auxiliary.voice_chat.base_url` | `''` | Custom endpoint for the model that answers in voice conversations. |
| Voice chat fields | `auxiliary.voice_chat.extra_body` | `{}` | Extra request fields sent to the voice conversation model. |
| Voice chat model | `auxiliary.voice_chat.model` | `''` | Model that answers in voice conversations. Empty picks one for you. |
| Voice chat provider | `auxiliary.voice_chat.provider` | see config | Service for the voice conversation model. Auto picks one for you. |
| Voice chat effort | `auxiliary.voice_chat.reasoning_effort` | see config | How long the voice conversation model thinks (default: off). Less thinking answers sooner. One of: none, minimal, low, medium, high, xhigh, max, ultra. |
| Voice chat timeout | `auxiliary.voice_chat.timeout` | see config | How long to wait for a voice reply before giving up. |
| Trim padding | `stt.cloud_trim_keep_ms` | see config | Quiet audio kept around speech when trimming before upload. |
| Trim silence | `stt.cloud_trim_silence` | see config | Cut silence from recordings before sending them to a cloud service. |
| Trim threshold | `stt.cloud_trim_threshold_db` | see config | Loudness below which audio counts as silence when trimming. |
| DeepInfra model | `stt.deepinfra.model` | `''` | Speech recognition model used with DeepInfra. Empty uses the service's default. |
| ElevenLabs endpoint | `stt.elevenlabs.base_url` | unset (the provider endpoint) | Custom endpoint for ElevenLabs speech recognition. Unset: the provider endpoint. |
| Label speakers | `stt.elevenlabs.diarize` | see config | Have ElevenLabs mark who is speaking in transcripts. |
| ElevenLabs language | `stt.elevenlabs.language_code` | `''` | Language code for ElevenLabs transcripts. Empty detects it. |
| ElevenLabs model | `stt.elevenlabs.model_id` | see config | Speech recognition model used with ElevenLabs. |
| Tag sounds | `stt.elevenlabs.tag_audio_events` | see config | Mark non-speech sounds such as laughter in ElevenLabs transcripts. |
| Groq language | `stt.groq.language` | `''` | Language hint for Groq transcripts. Empty detects it. |
| Groq model | `stt.groq.model` | see config | Speech recognition model used with Groq. |
| Recognition language | `stt.language` | see config | Language of your speech, as a language code such as en. Used when the chosen service has no language of its own. |
| Local prompt | `stt.local.initial_prompt` | `''` | Text that primes the on-device recognizer, such as names or jargon. |
| Local language | `stt.local.language` | `''` | Language hint for on-device recognition. Empty detects it. |
| Confidence cutoff | `stt.local.logprob_threshold` | see config | Average confidence below which on-device text is retried. |
| Local model | `stt.local.model` | see config | Size of the on-device speech recognition model. |
| No-speech cutoff | `stt.local.no_speech_prob_threshold` | see config | Chance of no speech above which a clip is treated as silence. |
| Unload when idle | `stt.local.unload_after_idle_seconds` | see config | Free the on-device recognizer after this long unused. Zero keeps it loaded. |
| Skip silence | `stt.local.vad` | see config | Detect speech and skip silent parts before on-device recognition. |
| Silence length | `stt.local.vad_min_silence_ms` | see config | How long a pause must last to count as a break in speech. |
| Mistral language | `stt.mistral.language` | `''` | Language hint for Mistral transcripts. Empty detects it. |
| Mistral model | `stt.mistral.model` | see config | Speech recognition model used with Mistral. |
| OpenAI recognition key | `stt.openai.api_key` | secret, in .env | Key for OpenAI speech recognition. |
| OpenAI recognition endpoint | `stt.openai.base_url` | unset (the provider endpoint) | Custom endpoint for OpenAI-compatible speech recognition. Unset: the provider endpoint. |
| OpenAI language | `stt.openai.language` | `''` | Language hint for OpenAI transcripts. Empty detects it. |
| Retry attempts | `stt.openai.max_retries` | see config | How many times to retry a failed recognition request. |
| OpenAI model | `stt.openai.model` | see config | Speech recognition model used with OpenAI. |
| OpenAI live model | `stt.openai.streaming_model` | see config | Model used for live transcription with OpenAI. |
| OpenAI timeout | `stt.openai.timeout` | see config | How long to wait for OpenAI to transcribe before giving up. |
| Recognition prompt | `stt.prompt` | unset (no static prompt) | Text that primes cloud recognition, such as names or jargon. Unset: no static prompt. |
| Live transcription | `stt.streaming` | see config | Show words as you speak when the chosen service supports it. |
| xAI recognition endpoint | `stt.xai.base_url` | unset (the provider endpoint) | Custom endpoint for xAI speech recognition. Unset: the provider endpoint. |
| xAI language | `stt.xai.language` | `''` | Language hint for xAI transcripts. Empty detects it. |
| xAI model | `stt.xai.model` | `''` | Speech recognition model used with xAI. Empty uses the service's default. |
| DeepInfra model | `tts.deepinfra.model` | `''` | Speech model used with DeepInfra. Empty uses the service's default. |
| DeepInfra speed | `tts.deepinfra.speed` | unset (the general speech speed) | How fast DeepInfra speaks. 1 is normal. Unset: the general speech speed. |
| DeepInfra voice | `tts.deepinfra.voice` | see config | Voice used with DeepInfra. |
| Edge speed | `tts.edge.speed` | unset (the general speech speed) | How fast the Edge voice speaks. 1 is normal. Unset: the general speech speed. |
| Edge voice | `tts.edge.voice` | see config | Voice used with Edge speech. |
| ElevenLabs endpoint | `tts.elevenlabs.base_url` | unset (the provider endpoint) | Custom endpoint for ElevenLabs speech. Unset: the provider endpoint. |
| ElevenLabs model | `tts.elevenlabs.model_id` | see config | Speech model used with ElevenLabs. |
| ElevenLabs live model | `tts.elevenlabs.streaming_model_id` | unset (the ElevenLabs model) | Speech model for ElevenLabs when speaking as text arrives. Unset: the ElevenLabs model. |
| ElevenLabs voice | `tts.elevenlabs.voice_id` | see config | Voice used with ElevenLabs, by its ID. |
| Gemini tone tags | `tts.gemini.audio_tags` | see config | Add tone cues to text before Gemini speaks it. |
| Gemini endpoint | `tts.gemini.base_url` | unset (the provider endpoint) | Custom endpoint for Gemini speech. Unset: the provider endpoint. |
| Gemini model | `tts.gemini.model` | see config | Speech model used with Gemini. |
| Gemini persona file | `tts.gemini.persona_prompt_file` | `''` | Path to a text file describing how Gemini should sound. |
| Gemini voice | `tts.gemini.voice` | see config | Voice used with Gemini. |
| Keep voice loaded | `tts.keep_warm_seconds` | see config | How long a local voice stays loaded after use. Zero unloads it at once. |
| Clean text | `tts.kittentts.clean_text` | see config | Tidy text before KittenTTS speaks it. |
| KittenTTS model | `tts.kittentts.model` | see config | On-device speech model used with KittenTTS. |
| KittenTTS speed | `tts.kittentts.speed` | see config | How fast KittenTTS speaks. 1 is normal. |
| KittenTTS voice | `tts.kittentts.voice` | see config | Voice used with KittenTTS. |
| MiniMax endpoint | `tts.minimax.base_url` | unset (the endpoint of the region) | Custom endpoint for MiniMax speech. Unset: the endpoint of the region. |
| MiniMax bitrate | `tts.minimax.bitrate` | see config | Audio quality of MiniMax speech, in bits per second. |
| MiniMax emotion | `tts.minimax.emotion` | see config | Emotion MiniMax puts in its speech, such as neutral or happy. |
| MiniMax group ID | `tts.minimax.group_id` | unset (none) | Account group ID sent to MiniMax. Unset: none. |
| MiniMax model | `tts.minimax.model` | see config | Speech model used with MiniMax. |
| MiniMax pitch | `tts.minimax.pitch` | see config | Raise or lower the MiniMax voice pitch. |
| MiniMax region | `tts.minimax.region` | unset (global, or China when only a China key is set) | Which MiniMax service region to use. One of: global, cn. Unset: global, or China when only a China key is set. |
| MiniMax sample rate | `tts.minimax.sample_rate` | see config | Audio sample rate for MiniMax speech, in Hz. |
| MiniMax speed | `tts.minimax.speed` | see config | How fast MiniMax speaks. 1 is normal. |
| MiniMax voice | `tts.minimax.voice_id` | see config | Voice used with MiniMax, by its ID. |
| MiniMax volume | `tts.minimax.vol` | see config | How loud MiniMax speaks. |
| Mistral endpoint | `tts.mistral.base_url` | unset (the provider endpoint) | Custom endpoint for Mistral speech. Unset: the provider endpoint. |
| Mistral model | `tts.mistral.model` | see config | Speech model used with Mistral. |
| Mistral voice | `tts.mistral.voice_id` | see config | Voice used with Mistral, by its ID. |
| Speech model | `tts.model` | unset (the plugin provider's model) | Speech model for plugin voice services. Unset: the plugin provider's model. |
| NeuTTS device | `tts.neutts.device` | see config | Where NeuTTS runs, such as cpu or cuda. |
| NeuTTS model | `tts.neutts.model` | see config | On-device speech model used with NeuTTS. |
| NeuTTS sample audio | `tts.neutts.ref_audio` | `''` | Path to a sample recording of the voice to copy. |
| NeuTTS sample text | `tts.neutts.ref_text` | `''` | Path to the transcript of the sample recording. |
| OpenAI speech key | `tts.openai.api_key` | secret, in .env | Key for OpenAI speech. |
| OpenAI speech endpoint | `tts.openai.base_url` | unset (the provider endpoint) | Custom endpoint for OpenAI-compatible speech. Unset: the provider endpoint. |
| Voice consent | `tts.openai.consent_attestation` | `''` | Consent statement sent when using a custom cloned voice. |
| OpenAI language | `tts.openai.language` | unset (no language hint) | Language hint sent with OpenAI-compatible speech. Unset: no language hint. |
| OpenAI model | `tts.openai.model` | see config | Speech model used with OpenAI. |
| Live sample rate | `tts.openai.pcm_sample_rate` | see config | Audio sample rate when OpenAI speech plays as it arrives, in Hz. |
| OpenAI speed | `tts.openai.speed` | unset (the general speech speed) | How fast OpenAI speaks. 1 is normal, from 0.25 to 4. Unset: the general speech speed. |
| OpenAI voice | `tts.openai.voice` | see config | Voice used with OpenAI. |
| Live latency boost | `tts.optimize_streaming_latency` | unset (no boost) | Trade quality for faster start when speech plays as it arrives. Used by xAI speech when its own setting is absent. Unset: no boost. |
| Output format | `tts.output_format` | unset (the default format of command providers) | Audio format produced by command-based voices. Unset: the default format of command providers. |
| Piper pace | `tts.piper.length_scale` | unset (the voice's own value) | Stretches or shortens Piper speech. Higher is slower. Unset: the voice's own value. |
| Piper variation | `tts.piper.noise_scale` | unset (the voice's own value) | How much natural variation Piper adds to the voice. Unset: the voice's own value. |
| Piper timing variation | `tts.piper.noise_w_scale` | unset (the voice's own value) | How much Piper varies the timing of words. Unset: the voice's own value. |
| Even out volume | `tts.piper.normalize_audio` | see config | Level out loudness in Piper speech. |
| Piper speaker | `tts.piper.speaker_id` | see config | Which speaker to use in a multi-speaker Piper voice. |
| Piper on GPU | `tts.piper.use_cuda` | see config | Run Piper on the graphics card instead of the processor. |
| Piper voice | `tts.piper.voice` | see config | Voice used with Piper. |
| Piper voices folder | `tts.piper.voices_dir` | unset (the voices folder in the icebath home) | Folder where Piper voices are stored and downloaded. Unset: the voices folder in the icebath home. |
| Piper volume | `tts.piper.volume` | see config | How loud Piper speaks. 1 is unchanged. |
| Speech speed | `tts.speed` | unset (normal speed) | How fast voices speak. 1 is normal. Services with their own speed setting use that instead. Unset: normal speed. |
| Live chunk size | `tts.streaming.min_len` | see config | Smallest piece of text spoken at once while streaming speech. |
| Live speech service | `tts.streaming.provider` | unset (the Speech voice service) | Service for speech that plays as the text arrives. Unset: the Speech voice service. |
| Speech voice name | `tts.voice` | unset (the plugin provider's voice) | Voice for plugin voice services. Unset: the plugin provider's voice. |
| xAI auto tags | `tts.xai.auto_speech_tags` | see config | Add expressive speech tags to text before xAI speaks it. |
| xAI speech endpoint | `tts.xai.base_url` | unset (the provider endpoint) | Custom endpoint for xAI speech. Unset: the provider endpoint. |
| xAI bitrate | `tts.xai.bit_rate` | see config | Audio quality of xAI speech, in bits per second. |
| xAI language | `tts.xai.language` | see config | Language xAI speaks in, as a language code. |
| xAI latency boost | `tts.xai.optimize_streaming_latency` | see config | Trade quality for faster start in xAI speech. Zero turns it off. |
| xAI sample rate | `tts.xai.sample_rate` | see config | Audio sample rate for xAI speech, in Hz. |
| xAI speed | `tts.xai.speed` | see config | How fast xAI speaks. 1 is normal. |
| xAI live endpoint | `tts.xai.streaming_url` | unset (the provider streaming endpoint) | Custom endpoint for xAI speech that plays as it arrives. Unset: the provider streaming endpoint. |
| xAI text cleanup | `tts.xai.text_normalization` | unset (the service default) | Have xAI spell out numbers and symbols before speaking. Unset: the service default. |
| xAI voice | `tts.xai.voice_id` | see config | Voice used with xAI. |
| Interrupt by talking | `voice.barge_in` | see config | Stop spoken replies when you start talking over them. |
| Interrupt grace | `voice.barge_in_grace_seconds` | see config | Time after a reply starts before talking can interrupt it. |
| Interrupt sensitivity | `voice.barge_in_threshold_multiplier` | see config | How much louder than the room you must be to interrupt. Zero turns the loudness check off. |
| Recording beep | `voice.beep_enabled` | see config | Play a beep when recording starts and stops. |
| Beep volume | `voice.beep_volume` | see config | How loud the recording beep is, from 0 to 1. |
| Longest recording | `voice.max_recording_seconds` | see config | Voice input stops listening after this long. |
| Silence length | `voice.silence_duration` | see config | Seconds of quiet before recording stops on its own. |
| Silence level | `voice.silence_threshold` | see config | Loudness below which audio counts as quiet. Raise it in noisy rooms. |
| Stop phrases | `voice.stop_phrases` | `- stop` | Spoken words that end voice conversation mode. |
| Thinking sound | `voice.thinking_sound` | see config | Play soft blips while waiting for a reply. |
| Confirmation frames | `wake_word.confirmation_frames` | see config | Checks in a row that must hear the Wake word before it triggers. |
| Wake microphone | `wake_word.input_device` | unset (the default microphone) | Microphone to listen on, by number or name. Unset: the default microphone. |
| Wake model file | `wake_word.openwakeword.model` | `''` | Path to the model file used for the Wake word. |
| Wake phrase | `wake_word.phrase` | see config | Phrase that wakes the assistant. |
| Porcupine keyword | `wake_word.porcupine.keyword` | see config | Built-in keyword Porcupine listens for. |
| Route by profile | `wake_word.profile_routing` | see config | Each profile can have its own Wake word and answers to it. |
| Wake engine | `wake_word.provider` | see config | Engine that listens for the Wake word. Auto picks one for you. |
| Wake sensitivity | `wake_word.sensitivity` | see config | How easily the Wake word triggers, from 0 to 1. Higher catches more but triggers by mistake more often. |
| Sherpa model folder | `wake_word.sherpa.model_dir` | `''` | Folder holding the Sherpa Wake word model. |
| Start fresh session | `wake_word.start_new_session` | see config | Begin a new session each time the Wake word is heard. |
| Wake listens in | `wake_word.surface` | see config | Which screen runs the Wake word listener. One of: auto, cli, tui, gui. |

## Images & media

Pictures and video the agent can make or look at.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Image generation | `image_gen.provider` | `''` | Which service draws pictures when the agent makes an image. One of: , fal, openai, openai-codex, xai, openrouter, deepinfra, krea, meta-ai. |
| Image model | `image_gen.model` | `''` | Model the image service draws with. Empty uses the service's default. |
| Image input | `agent.image_input_mode` | see config | How pictures you attach reach the model. One of: auto, native, text. Applies from the next turn. |
| Show images inline | `display.inline_images` | see config | Draw pictures inside the transcript instead of as links. One of: auto, kitty, unicode, off. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| OpenRouter model | `image_gen.openrouter.model` | `''` | Model OpenRouter draws with. Empty uses Image model, else OpenRouter's own pick. |
| OpenRouter aspect ratio | `image_gen.openrouter.aspect_ratio` | unset (picked from the request) | Exact aspect ratio to ask OpenRouter for, such as 16:9. Unset: picked from the request. |
| OpenRouter endpoint | `image_gen.openrouter.surface` | unset (picked per model) | Which OpenRouter endpoint image requests go to. One of: images, chat. Unset: picked per model. |
| OpenRouter time limit | `image_gen.openrouter.timeout` | unset (5 minutes) | Give up on an OpenRouter image request after this long. Unset: 5 minutes. |
| Attachment storage | `attachments.storage` | see config | Where files you attach to a prompt are saved. One of: icebath-home, workspace. |
| Image size budget | `vision.embed_target_bytes` | see config | Size images are shrunk to when kept in chat history. |
| Looks per image | `vision.max_calls_per_image` | unset (no limit) | Most times the same image may be analyzed in one chat. Zero means no limit. Unset: no limit. |
| Replayed images | `vision.replay_recent_images` | see config | How many earlier images are shown to the model again. Zero stops earlier images from being sent again. |
| Video generation | `video_gen.provider` | unset (the first available provider) | Plugin that makes videos when the agent asks for one. Unset: the first available provider. |

## Appearance

How the terminal UI looks.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Theme | `display.tui_theme` | see config | Light, dark, or match the terminal. One of: auto, light, dark. |
| Skin | `display.skin` | see config | The colour palette and symbols drawn on top of the theme. |
| Language | `display.language` | see config | Language of menus, notices and help. |
| Compact | `display.tui_compact` | see config | Less spacing so more of the transcript fits. |
| Timestamps | `display.timestamps` | see config | Show the time beside each message. |
| Mouse | `display.mouse_tracking` | see config | Which mouse actions the terminal UI takes over. Off leaves the mouse to the terminal, so you can select text the usual way. One of: all, buttons, wheel, off. |
| Busy indicator | `display.tui_status_indicator` | see config | Symbols of the spinner shown while the agent works. One of: unicode, emoji, ascii. |
| Pet | `display.pet.enabled` | see config | Show a small animated companion that reacts to the agent's work. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Classic CLI layout | `display.cli_layout` | see config | Glyph layout or boxed transcript in the classic command line. One of: modern, classic. |
| Pet drawing mode | `display.pet.render_mode` | see config | How the pet is drawn in your terminal. One of: auto, kitty, iterm, sixel, unicode, off. |
| Pet size | `display.pet.scale` | see config | How large the pet is drawn. Values are kept between 0.1 and 3. |
| Which pet | `display.pet.slug` | `''` | Which installed pet is shown. Empty shows none. |
| Text pet width | `display.pet.unicode_cols` | see config | Width of the pet in text blocks, in terminal columns. Zero works the width out from the pet size. |
| Status phrases | `display.status_phrases` | `{}` | Your own wording for the short progress messages in chat apps. |
| Time format | `display.timestamp_format` | `'%H:%M'` | How message times are written in the classic command line. Uses strftime codes. Times only show when timestamps are turned on. |
| Agents view hint | `display.tui_agents_nudge` | see config | Show a hint about the agents view in the terminal UI. |
| Native terminal mode | `display.tui_native` | see config | Let your terminal handle mouse selection and scrolling. Turns off mouse tracking in the terminal UI. The native flag on the command line does the same. |
| Paste collapse size | `paste_collapse_char_threshold` | see config | Pasted text longer than this is folded into a short placeholder. |
| Paste collapse lines | `paste_collapse_threshold` | see config | Pasted text with more lines than this is folded into a placeholder. |

## While it works

What you see and what Enter does while it works.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Detail level | `display.detail_level` | see config | How much of the agent's tool work and thinking you see. Sets tool progress and how far each part of the transcript is opened. Change single parts under advanced. One of: minimal, normal, detailed, everything, custom. |
| Show thinking | `display.show_reasoning` | see config | Show the model's thinking in the transcript. Chat apps hide thinking unless their own display settings turn it on. |
| Enter while busy | `display.busy_input_mode` | see config | What Enter does while the agent is still working. One of: queue, steer, interrupt. |
| Status bar | `display.tui_statusbar` | see config | Where the line with model, context use and cost sits. One of: top, bottom, off. |
| Status bar fields | `display.status_bar.fields` | `[]` | Which facts the status bar shows. Empty shows them all. Fields: model, context, context_pct, context_detail, cost, cwd, duration, latency, tps, cache_hit, compressions, bg_tasks, bg_subagents, battery, voice. |
| Battery | `display.battery` | see config | Show the laptop battery level in the status bar. |
| Bell when done | `display.bell_on_complete` | see config | Ring the terminal bell when the agent finishes a turn. |
| Bell on question | `display.bell_on_prompt` | see config | Ring the terminal bell when the agent needs your answer. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Tool progress | `display.tool_progress` | see config | Which tool calls get a line in the transcript while the agent works. One of: off, new, all, verbose, log. |
| Fold parts | `display.details_mode` | see config | Whether thinking, tools and subagents start hidden, folded or open. One of: hidden, collapsed, expanded. |
| Fold per part | `display.sections` | `{}` | Fold state of thinking, tools, subagents or activity on its own. |
| Full thinking | `display.reasoning_full` | see config | Show the model's thinking in full instead of a few lines. |
| Message reactions | `display.message_reactions` | see config | Tell the agent about reactions you add to its messages. Applies from the next turn. |
| Echo reasoning back | `agent.reasoning_echo` | see config | Send the model's earlier reasoning back to it on each request. Only needed for providers that require it. Applies to new sessions. |
| Arena layout | `display.arena` | `{}` | Settings for the arena view in the terminal UI. |
| Background job notices | `display.background_process_notifications` | see config | How much to report when a background command produces output. One of: concise, all, result, error, off. |
| Confirm steering | `display.busy_steer_ack_enabled` | see config | Confirm in chat when a message sent mid-turn steers the agent. |
| Busy chat text | `display.busy_text_mode` | unset (follows Enter while busy) | What a plain chat message does while the agent is working. Overrides Enter while busy for chat apps only. One of: interrupt, queue. Unset: follows Enter while busy. |
| Notice lifetime | `display.ephemeral_system_ttl` | see config | Seconds before short system notices in chat apps delete themselves. Zero keeps them. |
| Report failed edits | `display.file_mutation_verifier` | see config | List files the agent failed to edit at the end of a turn. |
| Final answer markup | `display.final_response_markdown` | see config | How markdown in the final answer is shown in the classic CLI. One of: strip, render, raw. |
| Friendly tool names | `display.friendly_tool_labels` | see config | Describe tool calls in plain words in chat apps. Applies from the next turn. |
| Inline diffs | `display.inline_diffs` | see config | Show file changes as a diff right in the transcript. |
| Mid-turn messages | `display.interim_assistant_messages` | see config | Show the agent's short updates while it works. Needs Show commentary to be on as well in the terminal UI. |
| Reasoning style | `display.reasoning_style` | see config | How shown reasoning is formatted in chat apps. One of: code, blockquote, subtext. |
| Show commentary | `display.show_commentary` | see config | Show the agent's running commentary between tool calls. |
| Spinner token flow | `display.spinner_token_flow` | see config | Show a live count of tokens flowing next to the spinner. |
| Stream replies | `display.streaming` | see config | Show replies as they are written instead of all at once. |
| Hide warning notices | `display.suppress_warning_notifications` | see config | Do not send warning notices to chat apps. |
| Tool preview length | `display.tool_preview_length` | see config | Characters of each tool call shown in chat progress lines. Zero shows the built-in default length. Applies from the next turn. |
| Progress command | `display.tool_progress_command` | see config | Allow the command that changes tool progress detail in chat apps. |
| Tool progress grouping | `display.tool_progress_grouping` | see config | Whether chat tool progress builds up in one message or many. One of: accumulate, separate. Applies from the next turn. |
| Explain odd endings | `display.turn_completion_explainer` | see config | Say why a turn ended when it stops without a normal answer. |
| Turn summary | `display.turn_summary` | see config | Show a one-line summary of time and activity after each turn. |
| Long call notices | `display.long_running_notifications` | `'on'` | Post a notice in chats while a long tool call keeps running. One of: on, generic, off. |
| Busy reply detail | `display.busy_ack_detail` | see config | Say what the agent is doing when a chat message arrives mid-turn. |
| Busy reply | `display.busy_ack_enabled` | see config | Reply in a chat when a message arrives while the agent works. |
| Clean up progress | `display.cleanup_progress` | see config | Delete tool progress messages from a chat once the reply is in. |
| Live status | `display.live_status` | see config | What the chat app's status line says while the agent works. One of: full, verb, off. |
| Thinking in chats | `display.thinking_progress` | see config | Post the model's thinking as progress messages in chats. |
| Busy message gap | `display.busy_text_debounce_seconds` | see config | Quiet time that closes a burst of chat messages sent mid-turn. Messages in one burst are queued as one. Restart the background service to apply. |
| Busy message cap | `display.busy_text_hard_cap_seconds` | see config | Longest a burst of chat messages sent mid-turn is held. Restart the background service to apply. |

## Memory & sessions

What the agent remembers and how old sessions are kept.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Memory | `memory.memory_enabled` | see config | Let the agent keep notes about your work between sessions. Applies to new sessions. |
| Learn about me | `memory.user_profile_enabled` | see config | Let the agent keep a short profile of you and your preferences. Applies to new sessions. |
| Ask before saving | `memory.write_approval` | see config | Ask you before the agent saves or changes a memory. |
| Resume last session | `display.tui_auto_resume_recent` | see config | Reopen your most recent session when the terminal UI starts. Restart the background service to apply. |
| Delete old sessions | `sessions.auto_prune` | see config | Delete sessions older than the keep time when the service starts. Restart the background service to apply. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Memory notices | `display.memory_notifications` | `'on'` | How much you are told when the agent saves to Memory. One of: on, verbose, off. |
| Memory size limit | `memory.memory_char_limit` | see config | Most characters the agent's own notes in Memory may hold. Applies to new sessions. |
| Memory reminder spacing | `memory.nudge_interval` | see config | Turns between reminders to the agent to save to Memory. Applies to new sessions. |
| Spill long recalls | `memory.prefetch_spill_enabled` | see config | Move long recalled Memory to a file instead of the prompt. Applies to outside Memory providers only. |
| Memory provider | `memory.provider` | `''` | Outside service used for Memory. Empty uses only the built-in Memory. |
| Profile size limit | `memory.user_char_limit` | see config | Most characters the notes about you in Memory may hold. Applies to new sessions. |
| Continue per terminal | `session.terminal_continue` | see config | Remember each terminal's session so it can be continued there. |
| Auto-archive sessions | `sessions.auto_archive` | see config | Archive sessions that have sat idle for a while. |
| Archive after idle | `sessions.auto_archive_days` | see config | How long a session must be idle before it is archived. Pinned sessions are never archived. |
| Resume interrupted turns | `sessions.auto_continue.enabled` | see config | Pick up a turn automatically after the app stopped mid-turn. |
| Resume window | `sessions.auto_continue.freshness_minutes` | see config | How recent an interrupted turn must be to resume itself. |
| Resume attempts | `sessions.auto_continue.max_attempts` | see config | Times one interrupted turn is resumed before giving up. |
| Search CJK text | `sessions.cjk_fts` | see config | Make Chinese, Japanese and Korean text searchable in sessions. |
| Search tune-up notice | `sessions.fts_optimize_notice` | see config | Whether to suggest tidying a large session search index. One of: advise, off. |
| Export size limit | `sessions.max_export_messages` | see config | Largest session, in messages, that can be exported in memory. Zero turns the guard off. |
| Resume size limit | `sessions.max_resume_messages` | see config | Largest session, in messages, that can be resumed. Zero turns the guard off. |
| Cleanup spacing | `sessions.min_interval_hours` | see config | Least time between automatic archive or cleanup runs. |
| Compact spacing | `sessions.min_vacuum_interval_days` | see config | Least time between compactions of the session database. |
| Abandoned session grace | `sessions.orphan_end_grace_s` | see config | How long an abandoned session waits before it is marked ended. Zero ends it right away. |
| Keep old sessions | `sessions.retention_days` | see config | Age after which old sessions are deleted by automatic cleanup. Applies only while Delete old sessions is on. |
| Slow search mark | `sessions.search_slow_ms` | see config | Searches slower than this are written to the log. |
| List subagent sessions | `sessions.show_subagents` | see config | Include sessions started by subagents in session lists. |
| Close stale sessions | `sessions.startup_orphan_sweep` | see config | At startup, close sessions left open by a crash or shutdown. |
| Compact after cleanup | `sessions.vacuum_after_prune` | see config | Shrink the session database after old sessions are deleted. |
| Idle disconnect limit | `sessions.ws_orphan_activity_stale_s` | see config | How long a disconnected session may sit idle before it is closed. Restart the background service to apply. |
| Disconnect grace | `sessions.ws_orphan_reap_grace_s` | see config | How long a session is kept after its window disconnects. Zero keeps it parked until it is reopened. Restart the background service to apply. |
| Sessions per window | `sessions.max_live_per_connection` | see config | Most sessions one window keeps open at once. |
| Open session limit | `sessions.max_live_per_profile` | see config | Most sessions open at once across every window of this profile. |

## Chat apps

Chat apps, scheduled jobs and their replies.

| Setting | Key | Default | What it does |
|---|---|---|---|
| Reply footer | `display.runtime_footer.enabled` | see config | Add a line with model and context use under chat app replies. |
| Human-like delay | `human_delay.mode` | `'off'` | Wait a moment before replying in chat apps, like a person typing. One of: off, natural, custom. Restart the background service to apply. |

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Voice per chat | `voice.chat_modes` | `{}` | Each chat's own choice of voice replies, set by /voice there. Chats missing here follow Speak replies. |
| Chat app display | `display.platforms` | `{}` | Display settings one chat app uses instead of the global ones. Keyed by chat app. Only the settings that say so take an override here. |
| Agent scheduling | `cron.allow_agent_scheduling` | see config | Let scheduled jobs create and change other scheduled jobs. |
| Catch up missed | `cron.catch_up_missed` | see config | Run jobs missed while the app was off, once when it returns. |
| Cleanup wait | `cron.cleanup_timeout_seconds` | see config | How long to wait for cleanup after a scheduled job finishes. Cleanup that runs longer is abandoned. |
| Delivery notices | `cron.delivery.notify` | see config | Send a notice to the chat when a scheduled result is delivered. |
| Failure nudge after | `cron.failure_nudge_threshold` | see config | Failed runs in a row before you get a nudge about a job. |
| Repeat alert gap | `cron.failure_repeat_alert_hours` | see config | Wait between repeat alerts for a job that keeps failing. Zero alerts on every failed run. |
| Stuck job limit | `cron.inflight_max_minutes` | unset (30 minutes) | Minimum time a running job gets before it counts as stuck. Unset falls back to the matching environment variable, then 30 minutes. Unset: 30 minutes. |
| Parallel jobs | `cron.max_parallel_jobs` | unset (no limit) | Most scheduled jobs that run at the same time. Zero also means no limit. The matching environment variable wins. Unset: no limit. |
| Media send wait | `cron.media_send_timeout_seconds` | see config | How long to wait when sending each file from a scheduled job. |
| Mirror results | `cron.mirror_delivery` | see config | Also add scheduled results to the chat session they were sent to. A job can override this. |
| Allow everyone | `buzz.allow_all_users` | see config | Buzz: let anyone in its channels talk to it. |
| Allowed people | `buzz.allowed_users` | `[]` | Buzz: public keys of people allowed to talk to it. Restart the background service to apply. |
| Channels | `buzz.channels` | `[]` | Buzz: channels it listens in. Restart the background service to apply. |
| Buzz program | `buzz.cli_path` | `''` | Buzz: path of the buzz program; empty finds it on the PATH. Restart the background service to apply. |
| Poll interval | `buzz.poll_interval` | unset (the built-in interval) | Buzz: seconds between checks for new messages when polling. Unset: the built-in interval. Restart the background service to apply. |
| React only to | `buzz.reaction_only_users` | `[]` | Buzz: public keys whose messages get a reaction but no reply. Restart the background service to apply. |
| Relay address | `buzz.relay_url` | `''` | Buzz: address of the relay the bot talks through. Restart the background service to apply. |
| Reply in thread | `buzz.reply_in_thread` | see config | Buzz: answer in a thread under the message; off answers in place. Restart the background service to apply. |
| Reply quoting | `buzz.reply_to_mode` | see config | Buzz: which parts of a reply point back at your message. One of: first, all, off. Restart the background service to apply. |
| Require mention | `buzz.require_mention` | see config | Buzz: answer in channels only when someone mentions it. Restart the background service to apply. |
| Connection type | `buzz.transport` | see config | Buzz: how it receives messages from the relay. One of: auto, websocket, poll. Restart the background service to apply. |
| Late start allowance | `cron.misfire_grace_minutes` | see config | How late a scheduled job may still start after its time. |
| Job model | `cron.model` | `''` | Model scheduled jobs use when they do not pick their own. Blank follows your main model. |
| Job model provider | `cron.model_provider` | `''` | Provider of the job model. Empty uses your main provider. |
| Kept job outputs | `cron.output_retention` | see config | Output files kept for each scheduled job. |
| Check before run | `cron.preflight` | see config | Check a job needs (skills, tools, logins) are ready before it runs. |
| Scheduler plugin | `cron.provider` | `''` | Scheduler plugin that fires jobs. Empty uses the built-in scheduler. |
| Safe restart only | `cron.require_restart_safe_scope` | see config | Refuse to run jobs that a restart could cut off. |
| Retry when offline | `cron.retry_unreachable` | see config | Retry a job later when the network or model was unreachable. |
| Job script limit | `cron.script_timeout_seconds` | see config | Longest a job pre-run script may run. |
| History open wait | `cron.session_db_timeout_seconds` | see config | How long a job waits to open the session history. Zero waits forever. The matching environment variable wins. |
| Direct send wait | `cron.standalone_send_timeout_seconds` | see config | How long to wait when a job sends outside a chat session. Zero or less uses the default. |
| Wrap results | `cron.wrap_response` | see config | Add a header and footer around scheduled results sent to chat. |
| Allow everyone | `discord.allow_all_users` | see config | Discord: let anyone who can see the bot talk to it. |
| Answer bots | `discord.allow_bots` | see config | Discord: which messages from other bots it answers. One of: none, mentions, all. |
| Allowed people | `discord.allow_from` | `[]` | Discord: user IDs or names allowed to talk to it; * allows everyone. Empty leaves access to pairing and the shared allow list. |
| Allowed channels | `discord.allowed_channels` | `[]` | Discord: only answer in these channel IDs. |
| Allowed roles | `discord.allowed_roles` | `[]` | Discord: server role IDs whose members may talk to it. |
| Ignored channels | `discord.ignored_channels` | `[]` | Discord: channel IDs or names it never reads. |
| No thread channels | `discord.no_thread_channels` | `[]` | Discord: channels where it answers in place, without a thread. |
| Approval mentions | `discord.approval_mentions` | see config | Discord: mention allowed users on approval requests. |
| Auto threads | `discord.auto_thread` | see config | Discord: start a thread for each new conversation. |
| Bots must mention | `discord.bots_require_inline_mention` | see config | Discord: other bots must mention it inside their message. |
| Channel prompts | `discord.channel_prompts` | `{}` | Discord: extra instructions for chosen channels. Maps channel ID to the instruction text. Restart the background service to apply. |
| DM role server | `discord.dm_role_auth_guild` | `''` | Discord: server whose roles allow direct messages. Blank lets roles apply only inside servers. Restart the background service to apply. |
| Free channel threads | `discord.free_response_auto_thread` | see config | Discord: start threads in channels where it replies without mention. |
| Free reply channels | `discord.free_response_channels` | `[]` | Discord: channel IDs where it replies without a mention. |
| Read recent history | `discord.history_backfill` | see config | Discord: read earlier messages for context when it joins a chat. |
| History messages | `discord.history_backfill_limit` | see config | Discord: most earlier messages read for context. |
| Attachment limit | `discord.max_attachment_bytes` | see config | Discord: largest file it downloads from a message. Zero means no limit. |
| Catch-up channels | `discord.missed_message_backfill.channels` | `[]` | Discord: channel IDs checked for messages missed while offline. Blank checks allowed and free reply channels. |
| Catch up messages | `discord.missed_message_backfill.enabled` | see config | Discord: answer messages missed while the service was down. Restart the background service to apply. |
| Catch-up scan size | `discord.missed_message_backfill.limit` | see config | Discord: most recent messages checked per channel when catching up. Restart the background service to apply. |
| Catch-up retries | `discord.missed_message_backfill.max_attempts` | see config | Discord: tries per missed message before giving up. Restart the background service to apply. |
| Catch-up replies | `discord.missed_message_backfill.max_dispatches` | see config | Discord: most missed messages answered per catch-up. Restart the background service to apply. |
| Catch-up window | `discord.missed_message_backfill.window_seconds` | see config | Discord: how far back missed messages are still answered. Restart the background service to apply. |
| Reactions | `discord.reactions` | see config | Discord: react to messages while working on them. |
| Require mention | `discord.require_mention` | see config | Discord: only answer in servers when mentioned. |
| Allowed server actions | `discord.server_actions` | `''` | Discord: server actions the agent may take. Comma separated action names. Empty allows all, and unknown names are ignored. |
| Threads need mention | `discord.thread_require_mention` | see config | Discord: also require a mention inside threads. |
| Voice idle leave | `discord.voice_channel_inactivity_timeout_seconds` | see config | Discord: leave a voice channel after this much silence. Zero never leaves. Restart the background service to apply. |
| Spoken acknowledgements | `discord.voice_fx.ack_enabled` | see config | Discord: say a short phrase when starting on a voice request. Restart the background service to apply. |
| Acknowledgement phrases | `discord.voice_fx.ack_phrases` | see config | Discord: phrases spoken while it works on a voice request. Blank list uses the built-in phrases. Restart the background service to apply. |
| Ambient sound | `discord.voice_fx.ambient_enabled` | see config | Discord: play background sound while it works on a voice request. Restart the background service to apply. |
| Ambient volume | `discord.voice_fx.ambient_gain` | see config | Discord: volume of the background sound. Restart the background service to apply. |
| Ambient sound file | `discord.voice_fx.ambient_path` | `''` | Discord: sound file played while it works. Empty uses the built-in sound. Restart the background service to apply. |
| Ambient duck volume | `discord.voice_fx.duck_gain` | see config | Discord: background volume while it speaks. Restart the background service to apply. |
| Voice effects | `discord.voice_fx.enabled` | see config | Discord: use sound effects and spoken acknowledgements in voice. Restart the background service to apply. |
| Speech lead silence | `discord.voice_fx.lead_silence_ms` | see config | Discord: silence added before speech so the start is not clipped. Restart the background service to apply. |
| Speech volume | `discord.voice_fx.speech_gain` | see config | Discord: volume of spoken replies. Restart the background service to apply. |
| Voice playback wait | `discord.voice_playback_timeout_seconds` | see config | Discord: shortest wait for spoken audio to finish playing. Restart the background service to apply. |
| Silent link limit | `discord.websocket_event_max_silence_seconds` | see config | Discord: reconnect when no events arrive for this long. Zero or less turns this check off. Restart the background service to apply. |
| Heartbeat reply limit | `discord.websocket_heartbeat_ack_max_age_seconds` | see config | Discord: reconnect when Discord stops answering heartbeats. Zero or less turns this check off. Restart the background service to apply. |
| Link check failures | `discord.websocket_liveness_failure_threshold` | see config | Discord: failed link checks in a row before reconnecting. Restart the background service to apply. |
| Link check interval | `discord.websocket_liveness_interval_seconds` | see config | Discord: time between connection health checks. Restart the background service to apply. |
| Link latency limit | `discord.websocket_max_latency_seconds` | see config | Discord: reconnect when the connection is slower than this. Zero or less turns this check off. Restart the background service to apply. |
| Footer fields | `display.runtime_footer.fields` | see config | Details shown in the footer line under chat replies. Choose from model, served_model, context_pct, latency and cwd, in order. |
| Allow all users | `gateway.allow_all_users` | see config | Let anyone message the chat apps, without an allowed list. |
| API output cap | `gateway.api_server.history_tool_output_max_chars` | see config | API server: longest tool output kept in saved conversation history. Zero keeps full tool output. Restart the background service to apply. |
| API parallel runs | `gateway.api_server.max_concurrent_runs` | see config | API server: most requests answered at the same time. Restart the background service to apply. |
| Auto merge profiles | `gateway.auto_multiplex_migration` | see config | Let the background service merge profiles into one when it can. |
| Bot loop pause | `gateway.bot_loop_guard.cooldown_seconds` | see config | How long a chat is ignored after a bot-to-bot loop is caught. |
| Bot loop guard | `gateway.bot_loop_guard.enabled` | see config | Stop replying when bots keep triggering each other in a chat. |
| Bot loop messages | `gateway.bot_loop_guard.max_events` | see config | Bot messages allowed in the window before the guard trips. |
| Bot loop window | `gateway.bot_loop_guard.window_seconds` | see config | Time span over which bot messages are counted. |
| Delivery records | `gateway.delivery_ledger` | see config | Record sent replies so a restart does not lose or repeat them. |
| Hang watchdog | `gateway.loop_watchdog` | see config | Detect a frozen background service and restart it. Restart the background service to apply. |
| Watchdog strikes | `gateway.loop_watchdog_max_strikes` | see config | Failed health probes in a row before the watchdog acts. Restart the background service to apply. |
| Watchdog interval | `gateway.loop_watchdog_probe_interval_s` | see config | Time between watchdog health probes. Restart the background service to apply. |
| Watchdog probe wait | `gateway.loop_watchdog_probe_timeout_s` | see config | How long a health probe may take before it counts as failed. Restart the background service to apply. |
| Managed workers | `gateway.managed_workers` | see config | Run chat sessions in separate worker processes under a fixed policy. |
| Incoming file limit | `gateway.max_inbound_media_bytes` | see config | Largest file accepted from a chat message. |
| Sendable folders | `gateway.media_delivery_allow_dirs` | `[]` | Extra folders whose files may be sent to chats. Files outside allowed folders are dropped. Restart the background service to apply. |
| Message timestamps | `gateway.message_timestamps.enabled` | see config | Show the agent the time each chat message was sent. |
| Merge profiles | `gateway.multiplex_profiles` | see config | Serve every profile from one background service. |
| Chat connect wait | `gateway.platform_connect_timeout` | see config | How long each chat app gets to connect at startup. Restart the background service to apply. |
| Profile routes | `gateway.profile_routes` | `[]` | Rules that send particular chats to a particular profile. Each entry names a chat and the profile that handles it. Restart the background service to apply. |
| Chat proxy | `gateway.proxy_url` | `''` | Proxy address for chat app connections. Empty connects directly. |
| Restart storm starts | `gateway.respawn_storm.max_starts` | see config | Starts allowed in the window before startup is delayed. Restart the background service to apply. |
| Restart storm window | `gateway.respawn_storm.window_seconds` | see config | Time span over which service starts are counted. Restart the background service to apply. |
| Restart loop gap | `gateway.restart_loop_guard.max_gap_seconds` | see config | Longest gap between restarts that still counts as a loop. |
| Restart loop count | `gateway.restart_loop_guard.max_restarts` | see config | Restarts in the window before the loop guard stops them. |
| Restart loop window | `gateway.restart_loop_guard.window_seconds` | see config | Time span over which restarts are counted. |
| Idle stop delay | `gateway.scale_to_zero.idle_timeout_minutes` | see config | How long the service waits with no work before it stops itself. |
| Service choice | `gateway.service_install_choice` | unset (not asked yet) | Your answer about installing the background service. Set by setup. Change it only to be asked again. One of: install, decline. Unset: not asked yet. |
| Stop grace time | `gateway.signal_interrupt_grace_timeout` | see config | How long a running turn gets to stop when the service is signaled. |
| Own service | `gateway.standalone` | see config | Run this profile's own background service instead of sharing one. Older option. Merge profiles is the better choice. |
| Startup watchdog | `gateway.startup_watchdog` | see config | Restart the service if it never finishes starting. Restart the background service to apply. The ICEBATH_STARTUP_WATCHDOG environment variable overrides it. |
| Startup time limit | `gateway.startup_watchdog_timeout_seconds` | see config | How long startup may take before the watchdog acts. Restart the background service to apply. The ICEBATH_STARTUP_WATCHDOG_TIMEOUT_S environment variable overrides it. |
| Strict file sending | `gateway.strict` | see config | Refuse to send files unless their folder is explicitly allowed. Restart the background service to apply. |
| System watchdog time | `gateway.systemd_watchdog_seconds` | unset (off) | Seconds between liveness pings to the system service manager. Unset: off. Restart the background service to apply. |
| Use proxy settings | `gateway.trust_env` | see config | Honor proxy and certificate environment variables for chat apps. |
| Trust recent files | `gateway.trust_recent_files` | see config | Allow sending just-created files from outside allowed folders. Restart the background service to apply. |
| All chats down | `gateway.on_all_adapters_down` | see config | What the background service does once every chat app has dropped. One of: exit, stay_alive. Restart the background service to apply. |
| Recent file window | `gateway.trust_recent_files_seconds` | see config | How recent a file must be to count as just created. Restart the background service to apply. |
| Room link address | `gateway.room_link_url` | unset (rooms are not offered to other machines) | Address other machines use to reach the rooms shared from here. Plain http is allowed only on this machine. A named profile without its own takes the main profile's. Unset: rooms are not offered to other machines. |
| Idle exit time | `gateway.unmanaged_idle_exit_seconds` | see config | A hand-started service quits after this long with no work. Zero never quits. |
| Write session index | `gateway.write_sessions_json` | see config | Keep a readable index file of chat sessions. Restart the background service to apply. |
| Room reply limit | `hosted_rooms.discussion.max_messages` | see config | Most messages the agents may post in one room discussion. |
| Room discussion rounds | `hosted_rooms.discussion.max_rounds` | see config | Most rounds of replies in one room discussion. |
| Longest typing pause | `human_delay.max_ms` | see config | Longest pause before a reply when custom pacing is on. Restart the background service to apply. |
| Shortest typing pause | `human_delay.min_ms` | see config | Shortest pause before a reply when custom pacing is on. Restart the background service to apply. |
| Allowed rooms | `matrix.allowed_rooms` | `[]` | Matrix: only answer in these room IDs. Restart the background service to apply. |
| Allowed people | `matrix.allowed_users` | `[]` | Matrix: user IDs allowed to talk to it; * allows everyone. Restart the background service to apply. |
| Auto threads | `matrix.auto_thread` | see config | Matrix: answer each new conversation in its own thread. Restart the background service to apply. |
| Device ID | `matrix.device_id` | `''` | Matrix: device ID the bot signs in as, kept for encryption. Restart the background service to apply. |
| Threads in DMs | `matrix.dm_mention_threads` | see config | Matrix: open a thread when mentioned in a direct chat. Restart the background service to apply. |
| Homeserver | `matrix.homeserver` | `''` | Matrix: address of the server the bot account lives on. Restart the background service to apply. |
| Ignored senders | `matrix.ignore_user_patterns` | `[]` | Matrix: patterns of user IDs whose messages it ignores. Restart the background service to apply. |
| Message length | `matrix.max_message_length` | unset (the server's limit) | Matrix: longest message it sends before splitting a reply. Unset uses the length the server allows. Unset: the server's limit. Restart the background service to apply. |
| Read notices | `matrix.process_notices` | see config | Matrix: treat notice messages, which bots send, as messages to it. Restart the background service to apply. |
| Conversation scope | `matrix.session_scope` | see config | Matrix: whether a conversation covers a whole room or one thread. One of: auto, room, thread. Restart the background service to apply. |
| Bot user ID | `matrix.user_id` | `''` | Matrix: user ID of the bot account, such as @bot:example.org. Restart the background service to apply. |
| Free reply rooms | `matrix.free_response_rooms` | `[]` | Matrix: room IDs where it replies without a mention. Restart the background service to apply. |
| Require mention | `matrix.require_mention` | see config | Matrix: only answer in rooms when mentioned. Restart the background service to apply. |
| Allowed channels | `mattermost.allowed_channels` | `[]` | Mattermost: only answer in these channel IDs. |
| Channel prompts | `mattermost.channel_prompts` | `{}` | Mattermost: extra instructions for chosen channels. Maps channel ID to the instruction text. Restart the background service to apply. |
| Free reply channels | `mattermost.free_response_channels` | `[]` | Mattermost: channel IDs where it replies without a mention. |
| Require mention | `mattermost.require_mention` | see config | Mattermost: only answer in channels when mentioned. |
| Platform hints | `platform_hints` | `{}` | Replace or extend the built-in tips about each chat app. Keyed by app name. Each entry replaces or extends that app's built-in hint. Applies to new sessions. |
| Allowed channels | `slack.allowed_channels` | `[]` | Slack: only answer in these channel IDs. |
| Channel prompts | `slack.channel_prompts` | `{}` | Slack: extra instructions for chosen channels. Maps channel ID to the instruction text. Restart the background service to apply. |
| Free reply channels | `slack.free_response_channels` | `[]` | Slack: channel IDs where it replies without a mention. |
| Skip others' mentions | `slack.ignore_other_user_mentions` | see config | Slack: ignore messages that mention someone else but not it. |
| Require mention | `slack.require_mention` | see config | Slack: only answer in channels when mentioned. |
| Mention-only channels | `slack.require_mention_channels` | `[]` | Slack: channel IDs that always need a mention. |
| Threads need mention | `slack.thread_require_mention` | see config | Slack: also require a mention inside threads. |
| Stream batch size | `streaming.buffer_threshold` | see config | Characters gathered before each streamed update is shown. Restart the background service to apply. |
| Stream cursor | `streaming.cursor` | `' ▉'` | Marker shown at the end of a reply while it streams. Restart the background service to apply. |
| Stream update gap | `streaming.edit_interval` | see config | Seconds between updates to a streamed reply. Restart the background service to apply. |
| Stream replies | `streaming.enabled` | see config | Show chat replies as they are written instead of all at once. Each chat app can switch it off separately. Restart the background service to apply. |
| Fresh final reply | `streaming.fresh_final_after_seconds` | see config | Send the final reply as a new message after this long. Zero edits the streamed message instead. Restart the background service to apply. |
| Stream method | `streaming.transport` | see config | How streamed replies are delivered. One of: auto, edit, draft, off. Restart the background service to apply. |
| Allowed chats | `telegram.allowed_chats` | `[]` | Telegram: only answer in these chat IDs. |
| Channel prompts | `telegram.channel_prompts` | `{}` | Telegram: extra instructions for chosen chats. Maps chat ID to the instruction text. Restart the background service to apply. |
| CJK rich text | `telegram.extra.allow_cjk_rich_messages` | see config | Telegram: allow rich formatting in Chinese, Japanese and Korean text. Restart the background service to apply. |
| Move DM topics | `telegram.extra.dm_topic_auto_transfer` | see config | Telegram: move a direct message conversation into its own topic. Restart the background service to apply. |
| Rich drafts | `telegram.extra.rich_drafts` | see config | Telegram: use rich formatting in streamed drafts. Restart the background service to apply. |
| Rich messages | `telegram.extra.rich_messages` | see config | Telegram: use rich formatting in replies. Restart the background service to apply. |
| Answer bots | `telegram.allow_bots` | see config | Telegram: which messages from other bots it answers. One of: none, mentions, all. |
| Allowed people | `telegram.allow_from` | `[]` | Telegram: user IDs allowed to talk to it; * allows everyone. Empty leaves access to pairing and the shared allow list. |
| Allowed topics | `telegram.allowed_topics` | `[]` | Telegram: the only forum topics it answers in. Each item is chat:topic. Empty allows every topic. |
| Bots must mention | `telegram.bots_require_mention` | see config | Telegram: other bots must mention it to get an answer. |
| Other bot mentions | `telegram.exclusive_bot_mentions` | see config | Telegram: skip messages that mention only another bot. |
| Free reply chats | `telegram.free_response_chats` | `[]` | Telegram: chats where it answers without being mentioned. |
| Free reply topics | `telegram.free_response_topics` | `[]` | Telegram: forum topics where it answers without being mentioned. Each item is chat:topic, such as -100123:7. |
| Allowed in groups | `telegram.group_allow_from` | `[]` | Telegram: user IDs allowed to talk to it in groups. |
| Open groups | `telegram.group_allowed_chats` | `[]` | Telegram: group chat IDs where everyone may talk to it. |
| Guest mode | `telegram.guest_mode` | see config | Telegram: answer when someone brings the bot into a chat as a guest. |
| Ignored topics | `telegram.ignored_threads` | `[]` | Telegram: forum topic numbers it never reads. |
| Mention words | `telegram.mention_patterns` | `[]` | Telegram: words that count as a mention besides the bot's name. Each item is a pattern matched against the message text. |
| Reply alerts | `telegram.notifications` | see config | Telegram: which replies make the phone buzz. One of: important, all. |
| Read group talk | `telegram.observe_unmentioned_group_messages` | see config | Telegram: keep group messages that do not mention it as context. It still answers only when mentioned. |
| Reactions | `telegram.reactions` | see config | Telegram: react to messages while working on them. |
| Require mention | `telegram.require_mention` | see config | Telegram: answer in groups only when someone mentions the bot. |
| Allowed people | `whatsapp.allow_from` | `[]` | WhatsApp: phone numbers allowed to talk to it; * allows everyone. |
| Direct chat access | `whatsapp.dm_policy` | see config | WhatsApp: who may talk to it in direct chats. One of: pairing, allowlist, open, disabled. Restart the background service to apply. |
| Free reply chats | `whatsapp.free_response_chats` | `[]` | WhatsApp: chats where it answers without being mentioned. |
| Allowed in groups | `whatsapp.group_allow_from` | `[]` | WhatsApp: group IDs or numbers allowed to talk to it in groups. |
| Group access | `whatsapp.group_policy` | see config | WhatsApp: who may talk to it in groups. One of: pairing, allowlist, open, disabled. Restart the background service to apply. |
| Mention words | `whatsapp.mention_patterns` | `[]` | WhatsApp: words that count as a mention besides its name. |
| Require mention | `whatsapp.require_mention` | see config | WhatsApp: answer in groups only when someone mentions it. |
| API version | `whatsapp_cloud.api_version` | `''` | WhatsApp Business: version of Meta's Graph API to call. Restart the background service to apply. |
| App ID | `whatsapp_cloud.app_id` | `''` | WhatsApp Business: ID of your Meta app. Restart the background service to apply. |
| Phone number ID | `whatsapp_cloud.phone_number_id` | `''` | WhatsApp Business: phone number ID from the Meta app dashboard. Restart the background service to apply. |
| Business account | `whatsapp_cloud.waba_id` | `''` | WhatsApp Business: ID of the business account. Restart the background service to apply. |
| Listen address | `whatsapp_cloud.webhook_host` | `''` | WhatsApp Business: address the message receiver listens on. Restart the background service to apply. |
| Webhook path | `whatsapp_cloud.webhook_path` | `''` | WhatsApp Business: path the message receiver answers on. Restart the background service to apply. |
| Listen port | `whatsapp_cloud.webhook_port` | unset (the built-in port) | WhatsApp Business: port the message receiver listens on. Unset: the built-in port. Restart the background service to apply. |
| Robot ID | `wecom.bot_id` | `''` | WeCom: ID of the smart robot the bot runs as. Restart the background service to apply. |
| Connection address | `wecom.websocket_url` | `''` | WeCom: address of the live connection; empty uses WeCom's own. Restart the background service to apply. |
| App ID | `wecom_callback.agent_id` | `''` | WeCom app: agent ID of the self-built app. Restart the background service to apply. |
| Company ID | `wecom_callback.corp_id` | `''` | WeCom app: ID of your company in WeCom. Restart the background service to apply. |
| Listen address | `wecom_callback.host` | `''` | WeCom app: address the message receiver listens on. Restart the background service to apply. |
| Listen port | `wecom_callback.port` | see config | WeCom app: port the message receiver listens on. Restart the background service to apply. |
| Strict mentions | `slack.strict_mention` | see config | Slack: answer in channels only when mentioned in that very message. |
| Refuse direct messages | `slack.disable_dms` | see config | Slack: ignore direct messages and answer only in channels. |
| Quiet thread follow-ups | `slack.thread_followup_silence` | see config | Slack: stay silent on thread replies not addressed to it. |
| Ignored channels | `slack.ignored_channels` | `[]` | Slack: channel IDs it never answers in. |
| Progress reactions | `slack.reactions` | see config | Slack: react to a message while working on it and when done. |
| Reaction triggers | `slack.reaction_triggers` | unset (reactions hand nothing over) | Slack: emoji that, added to a message, hand it to the agent. All, or a star, takes every emoji. Unset: reactions hand nothing over. |
| Reaction target | `slack.reaction_trigger_target` | unset (the channel the reaction was added in) | Slack: channel, or channel:thread, that reaction hand-offs go to. Unset: the channel the reaction was added in. |
| Other bots | `slack.allow_bots` | see config | Slack: whether messages from other bots are answered. One of: none, mentions, all. |
| Server address | `mattermost.url` | unset (not connected) | Mattermost: address of your Mattermost server. For example https://chat.example.com. The bot token stays with your other secrets. Unset: not connected. Restart the background service to apply. |
| Require mention | `dingtalk.require_mention` | see config | DingTalk: only answer in group chats when mentioned. |
| Wake patterns | `dingtalk.mention_patterns` | `[]` | DingTalk: patterns in a message that count as mentioning it. Regular expressions, matched without regard to case. Restart the background service to apply. |
| Free reply chats | `dingtalk.free_response_chats` | `[]` | DingTalk: group chat IDs where it replies without a mention. |
| Allowed chats | `dingtalk.allowed_chats` | `[]` | DingTalk: only answer in these group chat IDs. Empty allows every group chat. |
| Allowed users | `dingtalk.allowed_users` | `[]` | DingTalk: user or staff IDs allowed to use the bot. Empty or a star allows everyone. Restart the background service to apply. |
| Cloud | `feishu.domain` | see config | Feishu: whether the app is on Feishu in China or on Lark. One of: feishu, lark. Restart the background service to apply. |
| Connection | `feishu.connection_mode` | see config | Feishu: receive messages over a socket or through a web address. One of: websocket, webhook. Restart the background service to apply. |
| Other bots | `feishu.allow_bots` | see config | Feishu: whether messages from other bots are answered. One of: none, mentions, all. Restart the background service to apply. |
| Listener address | `bluebubbles.webhook_host` | see config | BlueBubbles: address the listener for new messages binds to. Restart the background service to apply. |
| Listener port | `bluebubbles.webhook_port` | see config | BlueBubbles: port the listener for new messages binds to. Restart the background service to apply. |
| Listener path | `bluebubbles.webhook_path` | see config | BlueBubbles: path the server sends new messages to. Restart the background service to apply. |
| Read receipts | `bluebubbles.send_read_receipts` | see config | BlueBubbles: mark messages as read once they are handled. Restart the background service to apply. |
| Require mention | `bluebubbles.require_mention` | see config | BlueBubbles: only answer in group chats when called by name. Restart the background service to apply. |
| Wake patterns | `bluebubbles.mention_patterns` | unset (the built-in patterns) | BlueBubbles: patterns in a group message that count as calling it. Regular expressions, matched without regard to case. Unset: the built-in patterns. Restart the background service to apply. |
| Service address | `weixin.base_url` | see config | Weixin: address of the iLink service the account talks to. Signing in by QR code sets it. Restart the background service to apply. |
| Media address | `weixin.cdn_base_url` | see config | Weixin: address media files are downloaded from. Restart the background service to apply. |
| Split lines | `weixin.split_multiline_messages` | see config | Weixin: send each line of a reply as its own message. Restart the background service to apply. |
| Direct messages | `weixin.dm_policy` | see config | Weixin: who may message the bot directly. One of: pairing, allowlist, open, disabled. Restart the background service to apply. |
| Allowed people | `weixin.allow_from` | `[]` | Weixin: user IDs allowed to message the bot directly. Restart the background service to apply. |
| Group chats | `weixin.group_policy` | see config | Weixin: which group chats the bot answers in. One of: pairing, allowlist, open, disabled. Restart the background service to apply. |
| Allowed groups | `weixin.group_allow_from` | `[]` | Weixin: group chat IDs the bot answers in. Restart the background service to apply. |
| Allowed people | `qqbot.allow_from` | `[]` | QQ: user OpenIDs allowed to message the bot directly. Restart the background service to apply. |
| Allowed groups | `qqbot.group_allow_from` | `[]` | QQ: group OpenIDs the bot answers in. Restart the background service to apply. |
| Skip stories | `signal.ignore_stories` | see config | Signal: ignore story posts from contacts. Restart the background service to apply. |
| Require mention | `signal.require_mention` | see config | Signal: only answer in group chats when mentioned. Restart the background service to apply. |
| Socket address | `yuanbao.ws_url` | see config | Yuanbao: address of the socket the bot receives messages on. Restart the background service to apply. |
| Service address | `yuanbao.api_domain` | see config | Yuanbao: address of the bot service that signs the bot in. Restart the background service to apply. |
| Service route | `yuanbao.route_env` | unset (the production service) | Yuanbao: which service environment the bot's requests go to. Unset: the production service. Restart the background service to apply. |
| Direct messages | `yuanbao.dm_policy` | see config | Yuanbao: who may message the bot directly. One of: pairing, allowlist, open, disabled. Restart the background service to apply. |
| Allowed people | `yuanbao.dm_allow_from` | `[]` | Yuanbao: user IDs allowed to message the bot directly. Restart the background service to apply. |
| Group chats | `yuanbao.group_policy` | see config | Yuanbao: which group chats the bot answers in. One of: pairing, allowlist, open, disabled. Restart the background service to apply. |
| Allowed groups | `yuanbao.group_allow_from` | `[]` | Yuanbao: group IDs the bot answers in. Restart the background service to apply. |
| Chat app connections | `platforms` | `{}` | Each chat app's connection: on or off, tokens and options. Restart the background service to apply. |
| Group per person | `gateway.group_sessions_per_user` | see config | In group chats, keep a separate conversation per person. Restart the background service to apply. |
| Thread per person | `gateway.thread_sessions_per_user` | see config | In threads, keep a separate conversation per person. Restart the background service to apply. |
| Keep local log | `gateway.always_log_local` | see config | Keep a local copy of chat app conversations. Restart the background service to apply. |
| Skip silence notes | `gateway.filter_silence_narration` | see config | Do not send replies that only say nothing happened. Restart the background service to apply. |
| Reset words | `gateway.reset_triggers` | see config | Messages that start a new conversation in a chat app. Restart the background service to apply. |
| Strangers | `gateway.unauthorized_dm_behavior` | see config | What the bot does when someone it does not know writes. One of: pair, ignore, decline. Restart the background service to apply. |
| Decline message | `gateway.unauthorized_dm_decline_message` | `''` | Reply sent to strangers when Strangers is Decline. Restart the background service to apply. |

## System

Timeouts, logging, updates and limits.

### Advanced

| Setting | Key | Default | What it does |
|---|---|---|---|
| Idle session lifetime | `agent.agent_cache.idle_ttl_secs` | see config | How long an unused agent stays loaded before it can be freed. |
| Frees per sweep | `agent.agent_cache.max_evictions_per_pass` | see config | Most idle agents freed in one cleanup sweep. |
| Loaded agents | `agent.agent_cache.max_size` | see config | How many agents stay loaded before the oldest idle ones are freed. |
| Memory ceiling | `agent.agent_cache.memory_high_mb` | see config | Memory use in MB above which idle agents are freed. Use auto to pick from the machine's memory, a number of MB, or off to never free by memory. |
| Keep recent agents | `agent.agent_cache.protect_recent` | see config | Most recently used agents that are never freed. |
| Model retries | `agent.api_max_retries` | see config | Times a failed model request is retried before giving up. Applies to new sessions. |
| Recovery attempts | `agent.auto_recovery_cycles` | see config | Times the agent tries to recover on its own after repeated failures. Applies to new sessions. |
| Startup wait | `agent.build_wait_timeout` | see config | How long a first prompt waits for a new agent to finish loading. |
| Question timeout | `agent.clarify_timeout` | see config | How long the agent waits for your answer to a question. |
| Scheduled job wait | `agent.cron_drain_timeout` | see config | How long a restart waits for running scheduled jobs to finish. Restart the background service to apply. |
| Resume window | `agent.gateway_auto_continue_freshness` | see config | How recent an interrupted chat app turn must be to resume itself. Restart the background service to apply. |
| Still-working notice | `agent.gateway_notify_interval` | see config | How often a chat app is told a long turn is still running. Zero turns the notices off. Restart the background service to apply. |
| Startup restore wait | `agent.gateway_startup_restore_drain_timeout` | see config | How long startup waits for interrupted turns to be restored. Restart the background service to apply. |
| Startup warmup wait | `agent.gateway_startup_warmup_timeout` | see config | How long startup waits for the background service to warm up. Restart the background service to apply. |
| Chat turn timeout | `agent.gateway_timeout` | see config | How long a chat app turn may run before it is stopped. Restart the background service to apply. |
| Chat turn warning | `agent.gateway_timeout_warning` | see config | How far into a chat turn a timeout warning is sent. Restart the background service to apply. |
| Turn lock timeout | `agent.gateway_turn_lease_timeout` | see config | How long a chat turn waits for the session to be free. Restart the background service to apply. |
| Local silence limit | `agent.local_stream_stale_timeout` | see config | How long a local model may stay silent before the request is dropped. Zero means no limit. |
| Reconnect alert delay | `agent.reconnect_attention_after` | see config | How long a chat app can stay disconnected before you are alerted. Zero or less turns the alert off. |
| Restart wait | `agent.restart_after_turn_timeout` | see config | How long a restart waits for running turns before going ahead. Restart the background service to apply. |
| Restart drain wait | `agent.restart_drain_timeout` | see config | How long shutdown waits for running work to wind down. Zero uses the built-in wait. Restart the background service to apply. |
| Repair limit | `agent.sanitizer_heal_escalation_threshold` | see config | Message repairs in a row before the agent reports a problem. |
| Stall timeout | `agent.session_stall_timeout` | see config | How long a chat session may sit with no activity before it is stuck. Restart the background service to apply. |
| Stream close wait | `agent.stream_drain_timeout` | see config | How long to wait for a finished model stream to close. |
| Stream time limit | `agent.stream_max_lifetime` | see config | Longest a single model response may keep streaming. Zero means no limit. |
| Model silence limit | `agent.stream_stale_timeout` | see config | How long a model may stay silent before the request is retried. Zero means no limit. |
| Hang check interval | `agent.turn_liveness.poll_s` | see config | How often a running turn is checked for being stuck. |
| Hang timeout | `agent.turn_liveness.timeout_s` | see config | How long a turn can make no progress before it is flagged as stuck. Zero or less turns the check off. |
| Memory trim cooldown | `context.memory_trim.cooldown_seconds` | see config | Minimum time between automatic memory cleanups. |
| Trim memory use | `context.memory_trim.enabled` | see config | Give unused process memory back to the system after agents close. |
| Trim log threshold | `context.memory_trim.info_log_min_delta_mb` | see config | Smallest memory cleanup, in MB, worth writing to the log. |
| Trim log spacing | `context.memory_trim.log_every_n` | see config | Log only every Nth memory cleanup. |
| Database journal mode | `database.journal_mode` | see config | How the session database writes to disk. Use delete on network drives or file systems that do not support write-ahead logs. One of: wal, delete. Restart the background service to apply. |
| Journal size cap | `database.journal_size_limit` | unset (database default) | Largest size the database write log may keep after cleanup. Unset: database default. Restart the background service to apply. |
| Checkpoint interval | `database.wal_autocheckpoint` | unset (database default) | Pages written before the database log is merged back. Unset: database default. Restart the background service to apply. |
| Database cache | `database.cache_size` | unset (database default) | Memory the session database keeps for recent pages. A positive number counts pages, a negative one counts KiB. Unset: database default. Restart the background service to apply. |
| Database memory map | `database.mmap_size` | unset (database default) | How much of the session database is read through memory mapping. Unset: database default. Restart the background service to apply. |
| Database temp storage | `database.temp_store` | unset (database default) | Where the session database keeps temporary tables. One of: 0, 1, 2. Unset: database default. Restart the background service to apply. |
| Database write safety | `database.synchronous` | unset (database default) | How carefully the session database waits for the disk after writes. macOS always keeps Full. One of: OFF, NORMAL, FULL, EXTRA. Unset: database default. Restart the background service to apply. |
| Live check timeout | `doctor.live_probe_timeout` | see config | How long each live connection check in the doctor may take. |
| Local model engine | `local_runtime.backend` | see config | Which engine runs local models. Auto picks one for this machine. |
| Local server ports | `local_runtime.detect_ports` | `[]` | Ports checked for an already running local model server. |
| Run local models | `local_runtime.enabled` | see config | Start and manage a local model server automatically. |
| Local models loaded | `local_runtime.models_max` | see config | Most local models kept loaded at once. The real limit can be lower when the graphics card cannot fit this many. |
| Local server port | `local_runtime.port` | see config | Port the local model server listens on. Zero lets the system choose a free port. |
| Old logs kept | `logging.backup_count` | see config | How many rotated copies of each log file are kept. Restart the background service to apply. |
| Log detail | `logging.level` | see config | How much the log files record. One of: DEBUG, INFO, WARNING, ERROR, CRITICAL. Restart the background service to apply. |
| Log file size | `logging.max_size_mb` | see config | Size at which a log file is rotated. Restart the background service to apply. |
| Retry failed servers | `lsp.broken_retry_seconds` | see config | How long to wait before retrying a code server that failed. Zero never retries until restart. |
| Code checking | `lsp.enabled` | see config | Check edited files for problems using language servers. |
| Skip folders | `lsp.exclude_roots` | `[]` | Folders, as glob patterns, where code checking is never used. |
| Server idle time | `lsp.idle_timeout` | see config | How long an unused language server runs before it is stopped. |
| Install language servers | `lsp.install_strategy` | see config | Whether missing language servers are installed for you. One of: auto, manual. |
| Node package manager | `lsp.package_manager` | see config | Tool used to install language servers written for Node. One of: npm, pnpm, yarn. |
| Language servers | `lsp.servers` | `{}` | Per-server overrides such as custom commands or disabling one. |
| Trusted folders | `lsp.trusted_workspaces` | `[]` | Extra folders where project-defined checkers may run. |
| Check wait mode | `lsp.wait_mode` | see config | How much to wait for after an edit before reporting problems. One of: document, full. |
| Check wait time | `lsp.wait_timeout` | see config | Longest wait for code problems after an edit. |
| Code server warmup | `lsp.warmup_timeout` | see config | Extra time a new language server gets to start before checks. Zero means no extra wait. |
| Active session limit | `max_concurrent_sessions` | unset (no limit) | Most chat sessions that may run a turn at once. Unset: no limit. Restart the background service to apply. |
| Live session limit | `max_live_sessions` | see config | Most sessions kept open before the least recent are closed. Zero removes the limit. |
| Online model list | `model_catalog.enabled` | see config | Fetch the list of available models from the online catalog. |
| Hidden providers | `model_catalog.excluded_providers` | `[]` | Providers left out of the model picker. |
| Provider overrides | `model_catalog.providers` | `{}` | Per-provider changes applied to the online model list. |
| Model list refresh | `model_catalog.ttl_minutes` | see config | How often the online model list is refreshed. |
| Model list address | `model_catalog.url` | see config | Web address the online model list is fetched from. |
| Model details address | `models_dev.url` | `''` | Web address for model details such as limits and prices. Empty uses the public models.dev registry. |
| Send telemetry | `monitoring.export.otlp.enabled` | see config | Send traces to an OpenTelemetry collector. Needs an address under the collector endpoint setting. |
| Collector address | `monitoring.export.otlp.endpoint` | `''` | Web address of the OpenTelemetry collector. |
| Collector headers | `monitoring.export.otlp.headers_env` | `{}` | Request headers for the collector, each read from a named variable. |
| Send diagnostic events | `monitoring.gateway_health_export.diagnostic_events_enabled` | see config | Include diagnostic events in the background service health export. |
| Export service health | `monitoring.gateway_health_export.enabled` | see config | Send background service health data to the telemetry collector. |
| Metrics interval | `monitoring.gateway_health_export.export_interval_seconds` | see config | How often health metrics are sent. |
| Log export interval | `monitoring.gateway_health_export.logs_export_interval_seconds` | see config | How often warning and error logs are sent. |
| Send health metrics | `monitoring.gateway_health_export.metrics_enabled` | see config | Include numeric health metrics in the export. |
| Export labels | `monitoring.gateway_health_export.resource_attributes` | `{}` | Labels such as an environment name attached to exported health data. Only service, cloud and environment names are sent; the service name is always icebath. |
| Send warning logs | `monitoring.gateway_health_export.warning_error_events_enabled` | see config | Include warning and error log events in the export. |
| Install ID | `monitoring.install_id` | `''` | Anonymous ID that labels this install in exported data. Created automatically when empty. |
| Prefer IPv4 | `network.force_ipv4` | see config | Try IPv4 addresses first, for networks with broken IPv6. Restart the background service to apply. |
| Profile setup offer | `onboarding.profile_build` | see config | Whether the agent offers to build your profile. One of: ask, off. |
| Seen tips | `onboarding.seen` | `{}` | Which first-run tips you have already seen. Filled in automatically. |
| Memory arenas | `runtime.malloc_arena_max` | see config | Cap on memory pools the system allocator creates. Lower values reduce memory growth on Linux. Restart the background service to apply. |
| Open file limit | `runtime.nofile_soft_limit` | see config | Open files the background service asks the system to allow. Restart the background service to apply. |
| Switch on update | `updates.auto_switch_parked_branch` | see config | Return to the update branch when updating from another one. |
| Backups kept | `updates.backup_keep` | see config | How many full pre-update backups are kept. |
| Check for updates | `updates.check` | see config | Look for new versions in the background. |
| Unattended local edits | `updates.non_interactive_local_changes` | see config | What an unattended update does with your changes to the source. One of: stash, discard. |
| Other-branch update | `updates.parked_branch_strategy` | see config | What an update does when a non-default branch is checked out. One of: switch, update_in_place. |
| Backup before update | `updates.pre_update_backup` | see config | What is backed up before an update runs. One of: off, quick, full. |
| Refresh computer driver | `updates.refresh_cua_driver` | see config | Update the computer-control helper along with the app. |
| Tool batch limit | `timeouts.tools.concurrent_batch` | see config | Longest a batch of tool calls run at the same time may take. Zero means no limit. Applies from the next turn. |
| Single tool limit | `timeouts.tools.sequential_call` | unset (the tool batch limit) | Longest one tool call run on its own may take. Zero means no limit. Unset: the tool batch limit. Applies from the next turn. |
| Request time limit | `agent.api_timeout` | see config | How long one model request may take before it is given up. A provider's own request timeout under Providers wins over this. Applies from the next turn. |
| Silent request limit | `agent.api_call_stale_timeout` | unset (picked from the model and request size) | How long a model may stay silent on a request that is not streamed. Unset picks it from the model and the request size. Zero means no limit. A provider's own stale timeout wins over this. Unset: picked from the model and request size. Applies from the next turn. |

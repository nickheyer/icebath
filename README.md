# icebath
A tiling agentic harness and netrunner toolkit for a deep dive beyond the blackwall.

## Install

```
go install github.com/nickheyer/icebath/cmd/icebath@latest
```

## Usage

```
icebath [flags] [prompt]
  -profile name      endpoint profile: anthropic, openai, openai-chat, google, mistral, openrouter, ollama, bedrock, nebu
  -model id          model the profile serves
  -mode name         default, acceptEdits, plan or bypassPermissions
  -resume id         continue a stored session
  -mission text      start a mission with an objective
  -kind name         kind the session runs as, from extensions or kind files
  -workflow name     run a workflow with the prompt as its argument
  -k setting=value   max_tokens, max_rounds, temperature, compact_threshold, prune_after, theme, motion, cascade, extra.<field>
  -config path       extra configuration file
  -dir path          working directory of the shell
  -root path         project root for hooks, rules and servers
  -theme name        blackwall, light, contrast or plain
  -print             no terminal, every event as a JSON line
  -headless          no terminal, the answer as text
  -yes               approve every tool call without a terminal
  -sessions          list the stored sessions of this project
  -all               with -sessions, list the sessions of every project

export ANTHROPIC_API_KEY=...
icebath "check disk usage"
echo "summarize go.mod" | icebath -print -yes
icebath -mission "map the open ports on 10.0.0.0/24"
icebath -workflow loop "make every test pass"
icebath -kind critic "review pkg/agent"
```

Configuration Files: `~/.config/icebath/config.json` or `.icebath/config.json`, `rules.json`,`hooks.json`, `kinds/*.json`, `~/.config/icebath/profiles.json`, `~/.config/icebath/keybinds.json`

Projects: sessions are stored together under `~/.config/icebath/sessions` and each carries the project root it was started in. The sessions panel and `-sessions` show the sessions of the current project unless widened to every project. Each project keeps its workspace layout under `~/.config/icebath/projects/<name>-<hash>/layout.json`. The first icebath open on a project restores and keeps that layout. A second one on the same project starts empty and keeps none. A session open in one icebath is leased to it, so another icebath sees it marked as open elsewhere and can fork it but not resume it.

Templates: `/template save <name>` keeps every workspace as it stands, its panels and splits, under `~/.config/icebath/templates.json`. Each session card stands for a fresh session on the profile, model and kind the session ran on. `/template apply <name>` adds those workspaces beside the open ones. `/template default <name>` makes a template the start of every project that has no layout yet, and `/template delete <name>` forgets one. The palette offers the same under Template. A profile, model, kind or panel a template names that is gone is skipped without a word, the default endpoint standing in for the session.

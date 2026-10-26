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
  -k setting=value   max_tokens, max_rounds, temperature, compact_threshold, theme, motion, cascade, extra.<field>
  -config path       extra configuration file
  -dir path          working directory of the shell
  -root path         directory every file path must stay inside
  -theme name        blackwall, light, contrast or plain
  -print             no terminal, every event as a JSON line
  -headless          no terminal, the answer as text
  -yes               approve every tool call without a terminal
  -sessions          list stored sessions

export ANTHROPIC_API_KEY=...
icebath "check disk usage"
echo "summarize go.mod" | icebath -print -yes
icebath -mission "map the open ports on 10.0.0.0/24"
```

Configuration Files: `~/.config/icebath/config.json` or `.icebath/config.json`, `rules.json`,`hooks.json`, `~/.config/icebath/profiles.json`, `~/.config/icebath/keybinds.json`, `~/.config/icebath/layout.json`


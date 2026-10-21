# icebath
The netrunner toolkit for a deep dive beyond the blackwall, an agent harness with a deck of animated cards over any model endpoint.

## Install

```
go install github.com/nickheyer/icebath/cmd/icebath@latest
```

## Usage

```
icebath [flags] [prompt]
  -profile name      endpoint profile: anthropic, openai, openai-chat, google, mistral, openrouter, ollama, bedrock
  -model id          model the profile serves
  -mode name         default, acceptEdits, plan or bypassPermissions
  -resume id         continue a stored session
  -mission text      start a mission with an objective
  -k knob=value      max_tokens, max_rounds, temperature, compact_threshold, theme, motion, extra.<field>
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

Configuration is read from `~/.config/icebath/config.json`, then `.icebath/config.json` in the working directory, with `rules.json` and `hooks.json` beside each. Extra endpoint profiles go in `~/.config/icebath/profiles.json`, a list or `{"default": "name", "profiles": [...]}` where `default` picks the profile used when none is given.

```go
app, err := harness.Build(ctx, cfg, send, harness.BuildOptions{})
run, err := app.Run(ctx, "", []protocol.Part{protocol.Text{Text: "check disk usage"}})
```

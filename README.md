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
icebath -workflow loop "make every test pass"
icebath -kind critic "review pkg/agent"
```

Configuration Files: `~/.config/icebath/config.json` or `.icebath/config.json`, `rules.json`,`hooks.json`, `kinds/*.json`, `~/.config/icebath/profiles.json`, `~/.config/icebath/keybinds.json`, `~/.config/icebath/layout.json`

## Extend

An extension is a Go package that registers itself and declares into a host. Link it with one import in `cmd/icebath/extensions.go` and build.

```go
package critic

import (
	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
)

type critic struct{}

func init() { ext.Register(critic{}) }

func (critic) Manifest() ext.Manifest {
	return ext.Manifest{Name: "critic", Version: "1.0.0", Description: "read only reviewer"}
}

func (critic) Attach(h *ext.Host) error {
	h.Kind(harness.Kind{Name: "critic", Description: "reads code and reports problems", Tools: []string{"Read", "Grep", "Glob", "Record"}})
	return nil
}
```

```
h.Protocol(p)       wire protocol          h.Kind(k)          session type, also a subagent kind
h.Profile(p)        endpoint profile       h.Workflow(w)      procedure over sessions, /name and palette
h.Vendor(v)         model lister           h.Setting(s)       config key, -k, settings tab
h.Auth(kind, b)     credential kind        h.Tools(build)     tools built with the app
h.Catalog(m...)     model metadata         h.Hook(build)      agent hook per agent
h.Command(c)        palette, slash, key    h.Prompt(f)        system prompt fragment
h.Panel(p)          card by key            h.Theme(fn)        palette
h.Context(n, d)     keymap context         h.Action(a)        key action in it
h.Observe(fn)       every harness message
```

Kinds also load without code from `kinds/*.json` with the fields `name`, `description`, `system`, `append`, `tools`, `deny`, `model`, `max_rounds`. The linked `extensions/loop` package is the reference extension.


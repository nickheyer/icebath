# icebath
The netrunner toolkit for a deep dive beyond the blackwall.

```
pkg/
 - agent: turn loop, stream the model, gate and run its tool calls, append results, repeat until it stops
   - hooks: lifecycle hooks run as external commands fed JSON on stdin
   - compact: summarize old turns past a context threshold, strip stale thinking
   - subagent: an agent exposed as a tool so a parent can delegate
 - policy: allow, deny or ask decision per tool call, rules plus an approver for the ask path
   - rules: Tool(pattern) allow, deny and ask lists loaded from settings JSON
   - mode: plan, acceptEdits and bypass modes layered over a base policy, used before destructive or open world tools
   - judge: model backed reviewer that approves, denies or escalates
   - tty: terminal prompt answering yes, no, always or never
 - profiles: declarative endpoint configs, credential sources and vendor conventions
   - anthropic, openai, google, mistral, openrouter, ollama, bedrock: defaults, model listing, knobs
 - protocol: neutral request, response and event model, one folder per wire protocol
 - provider: HTTP transport for one endpoint, credential carrier, retries
   - sigv4: AWS Signature Version 4 signing with the default credential chain
   - record: cassette transport that records exchanges and replays them offline
   - failover: tries providers in order until one answers
 - session: conversation transcript, usage totals, fork, compaction, markdown export
   - jsonfile: one JSON document per session
   - jsonl: append only log that survives a crash mid write
 - tool: tool interface and registry, read only calls in parallel, mutating calls one at a time, idempotents run once
   - files: Read, Write, Edit, Glob and Grep inside an optional root
   - shell: Bash with a persistent working directory, timeouts and background jobs
   - web: WebFetch with HTML to text and an optional model that answers a prompt
   - mcp: Model Context Protocol client over stdio and streamable HTTP
 - ui: terminal widgets, card deck, spring animation, text effects, theme
```

## Install

```
go get github.com/nickheyer/icebath
```

## Usage

```go
prov, _ := profiles.Lookup("anthropic")
model, err := prov.Open(ctx)

sh, _ := shell.New(shell.Options{})
var tools tool.Registry
tools.Add(files.All(files.Options{Root: "."})...)
tools.Add(sh.Tools()...)

perms, _ := rules.New(rules.Set{Allow: []string{"Read", "Glob", "Grep", "Bash(git *)"}})
gate := policy.Gate{Policy: perms, Approver: &policy.Memory{Ask: tty.New(os.Stdin, os.Stdout), Key: perms.Key}}

a := &agent.Agent{Model: model, ModelID: "claude-opus-5", Tools: &tools, Gate: gate, Session: session.New(), MaxTokens: 8192}
resp, err := a.Run(ctx, []protocol.Part{protocol.Text{Text: "check disk usage"}}, nil)
```

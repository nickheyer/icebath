# icebath port

Hermes Agent rewritten as a Go program named icebath. This file holds the rules of the port and the layout the repository ends in.

## Rules

- Every feature is implemented in Go. A feature is its black-box inputs and outputs. Everything between them is free to change or disappear.
- No Python and no Node, in the repository or at runtime. The one exception is the Vite toolchain that builds the browser code under `ui/`.
- Nothing is skipped as impossible in Go. Only Nick rules a feature out.
- Python and Node boilerplate has no Go counterpart and is deleted: virtualenvs, package managers, interpreter probes, SDK import shims, library emulation.
- Go code is written as Go. No emulation of Python or Node libraries, exception names, or module layouts.
- Wails is the desktop shell. Electron is gone.
- No Hermes or Nous naming, links, or branding. The product is icebath, the repository is github.com/nickheyer/icebath, the site is icebath.app, the developer organization is heyer.app.
- No tests until the port is complete. No notes folders, ledgers, line counts, or coverage tracking.
- `references/` is the behavior spec while the port runs. It is untracked and is deleted when the port ends.

## End state

```
icebath/
  cmd/
    icebath/                 product binary: argument handoff and construction of every service
    icebath-init/            container PID 1: volume bootstrap, service supervision, zombie reaping
  internal/                  all product code, one package per folder
    acp/                     Agent Client Protocol
      protocol/              JSON-RPC schema, connection, router
      server/                editor-facing server that exposes sessions to ACP clients
      client/                drives external ACP agent programs as a model provider
    agent/                   conversation turn loop: requests, tool rounds, interrupts, budgets, fallback
      compress/              context compression: triggers, summarization, commit
      prompt/                system prompt, project instruction files, @references
      delegate/              subagents: spawn, progress, results, worktrees
      review/                background review after a turn
      moa/                   mixture-of-agents orchestration
      goal/                  goal loop, judge, completion gates
      automation/            loop and heartbeat schedules bound to a session
      title/                 session title generation
      verify/                verification evidence gate and run-recipe smoke tests
    audio/                   microphone and speaker streams through PortAudio
    backup/                  home directory backup, restore, SQLite snapshots
    botdesktop/              per-profile headless desktop for computer use: display, lease, screen relay
    botrelay/                bot-to-bot direct messages across gateways
    cli/                     command tree, flags, help, shell completion
      chat/                  interactive chat: line editor, completion menu, status bar, streamed output
      slash/                 slash command registry and handlers shared by chat, terminal UI, gateway
      setup/                 setup wizards for models, tools, platforms, terminal
      doctor/                doctor, status, dump, debug, logs commands
      prompt/                menus, checklists, confirmations
      render/                terminal output: markdown, tables, diffs, progress, color
    config/                  config file load, save, defaults, env file, migrations, skins
      remote/                remotely managed config backend
    container/               container image runtime: stage hooks, user remap, health check
    cron/                    scheduled jobs: store, due scan, execution ledger, delivery, retry ladders
    dashboard/               web dashboard HTTP server
      api/                   REST and WebSocket routes by area: config, sessions, models, cron, plugins, files
      auth/                  auth gate and providers: basic, OIDC, session token
      webapp/                serves the desktop UI to a browser, preview proxy, staged uploads
    dl/                      shared library loading without cgo
    fileutil/                atomic writes, file locks, safe archive extraction
    gateway/                 messaging gateway: config, platform adapter contract, registry
      session/               session store, routing keys, authority, policy, ingress, workers, recovery
      runner/                gateway daemon: adapter lifecycle, turn pipeline, startup, shutdown
      delivery/              outbound router, delivery ledger, channel directory, standalone senders
      stream/                streamed replies and streamed speech
      relay/                 relay connector transport, provisioning, egress
      rooms/                 hosted rooms shared by several gateways
      api/                   OpenAI-compatible API server platform
      webhook/               webhook platform, filters, route scripts, subscriptions
      pairing/               DM pairing codes, owner identity, access rules
      service/               install and control as systemd, launchd, Task Scheduler service
      lifecycle/             pid and lock files, runtime status, readiness, drain, restart
      control/               owner control socket, gateway client, runtime discovery
      media/                 inbound and outbound media: fetch, cache, policy, stickers
      platform/              one folder per messaging platform, each a complete adapter
        a2a/                 agent-to-agent JSON-RPC server and client
        bluebubbles/         iMessage through BlueBubbles
        buzz/                Buzz relay chat over Nostr
        dingtalk/            DingTalk stream bot with AI cards
        discord/             Discord bot with slash commands, threads, voice
        email/               IMAP receive and SMTP reply
        feishu/              Feishu and Lark bot, cards, document comments
        googlechat/          Google Chat bot with per-user OAuth
        irc/                 IRC client
        line/                LINE bot
        matrix/              Matrix client with end-to-end encryption
        mattermost/          Mattermost bot
        ntfy/                ntfy topic chat
        photon/              iMessage through Photon
        qqbot/               QQ bot with voice and QR onboarding
        raft/                Raft channel agent
        signal/              Signal through signal-cli
        simplex/             SimpleX Chat
        slack/               Slack socket-mode bot with assistant threads
        sms/                 Twilio SMS
        teams/               Microsoft Teams bot
        telegram/            Telegram bot with topics
        wecom/               WeCom AI bot and callback mode
        weixin/              personal WeChat
        whatsapp/            WhatsApp multi-device client
        whatsappcloud/       WhatsApp Business Cloud API
        yuanbao/             Yuanbao bot with group tools and stickers
    gitcred/                 git and GitHub credentials the user already owns
    home/                    home directory layout, scratch space, permissions
    hostinfo/                operating system, CPU, GPU, memory, battery, container facts
    httpclient/              shared HTTP client: proxy, trust store, retry, decompression
    i18n/                    message lookup over the embedded locale catalogs
    imaging/                 image decode, resize, quantize, QR codes
    install/                 install ladder, PATH wiring, desktop install, uninstall
    kanban/                  kanban board: store, dispatcher, worker spawn, agent tools
      api/                   board routes for the dashboard and desktop app
    logging/                 rotating log files, session log export
    lsp/                     language server client, diagnostics, server install recipes
    mcp/                     Model Context Protocol client: transports, discovery, sampling, elicitation
      oauth/                 OAuth for MCP servers: PKCE, registration, refresh, device flow
      connector/             hosted app connectors and their accounts
    media/                   shared media types and cache
      imagegen/              image generation, one file per backend
      videogen/              video generation, one file per backend
      tts/                   text to speech: cloud engines, local engines, streaming
      stt/                   speech to text: cloud engines, local engines, streaming
      voice/                 voice mode and live voice turns
      wakeword/              wake word detection engines
      speech/                on-device speech models through sherpa-onnx
    memory/                  memory manager, built-in MEMORY.md store, memory tool
      provider/              one folder per external memory backend
        byterover/           ByteRover
        hindsight/           Hindsight
        holographic/         Holographic
        honcho/              Honcho
        mem0/                Mem0
        openviking/          OpenViking
        retaindb/            RetainDB
        supermemory/         Supermemory
      curator/               background skill and memory maintenance, snapshots, rollback
      insights/              usage insights, learning graph, journey timeline
    model/                   model routing: resolve, fallback chain, retry, error classes, token estimates
      wire/                  wire protocols
        anthropic/           Anthropic Messages
        openai/              OpenAI Chat Completions and Responses
        gemini/              Gemini generateContent
        bedrock/             Bedrock Converse
        codex/               Codex app-server sessions
      auth/                  credential store, credential pool, OAuth device and PKCE flows
        aws/                 AWS credential chain, SigV4, event stream
        gcp/                 Google service account, user, metadata, workload identity
        azure/               Entra credentials and the Windows account broker
      aux/                   side-model client for summaries, vision, titles
      catalog/               model lists, context windows, capabilities, pricing
      providers/             one file per model provider profile
      local/                 managed llama.cpp runtime, model files, hardware fit, local server discovery
      portal/                Icebath Portal account: login, plan, credits, managed tool gateway
      proxy/                 local OpenAI-compatible proxy and the egress credential proxy
      secret/                secret sources: env, command, 1Password, Bitwarden
      usage/                 account usage windows and spend display
    monitor/                 gateway and cron health metrics, OTLP export
    nemorelay/               NeMo Relay runtime speaking its protocol natively
    pet/                     animated pets: store, sprite render, generation
    plugin/                  plugin manager: manifests, discovery, install, enable, validate
      runtime/               WebAssembly and external-program plugin hosts and their wire protocol
      hook/                  hook events, shell hooks, outbound webhooks
      catalog/               installable plugin index and search
      desktop/               desktop plugin halves served to the desktop app
      builtin/               plugins compiled into the binary
        achievements/        achievements dashboard tab
        delegation/          default delegation provider
        designskills/        bundled design skills and their lint hooks
        diskcleanup/         temp file tracking and cleanup
        googlemeet/          Google Meet bot
        securityguidance/    security guidance hook
        teamspipeline/       Teams meeting transcript pipeline
    proc/                    process groups, process table, tree kill
    profile/                 named profiles: create, clone, export, import, delete
    research/                batch and dataset tooling
      batch/                 batch runner over prompt datasets
      trajectory/            trajectory compression to a token budget
      swe/                   single-task software engineering runner
    rpc/                     JSON-RPC backend for every UI: contracts, transports, replay
      methods/               method handlers by area: session, prompt, tools, config, voice, pets
    shell/                   shell word splitting and quoting
    skill/                   skill index, view and manage tools, usage, sync, bundles
      hub/                   skill sources and installs
      guard/                 skill security scan, lock, change ledger
      kit/                   Go implementations of the helper programs bundled skills call
    state/                   SQLite session store: schema, migrations, messages, compression state
      search/                full-text, trigram, and CJK search
    syntax/
      python/                Python parser for linting written files and auditing skill scripts
    telemetry/               opt-in usage metrics: local store, daily upload
    tokenizer/               tokenizer.json, SentencePiece, tiktoken loaders
    tool/                    tool registry, dispatch, parallel executor, schemas, toolsets
      approval/              dangerous command detection, approval gates, permission modes
      browser/               browser tool over CDP, launchers, cloud browsers, autofill
      builtin/               small tools: clarify, todo, session search, send message
      checkpoint/            shadow-git checkpoints and rollback
      codeexec/              execute_code sandbox with the tool bridge
      computeruse/           computer use driver
      connector/             connector tools
      desktopui/             tools that drive the desktop app: preview, tour, theme, projects
      docconv/               document to Markdown conversion
      env/                   execution environments: local, docker, ssh, modal, daytona, vercel
      file/                  read, write, patch, search, path guards, lint
      process/               background processes, PTY, owner-death supervision
      redact/                secret redaction
      terminal/              terminal tool, pre-exec guards, sudo, background runs
      vision/                image and video analysis
      web/                   web search and extract backends, SSRF-safe client, site policy
      worktree/              git worktrees and locks
    toolchain/               pinned external binaries: catalog, lock, download, verify, store
    tui/                     full-screen terminal UI: gateway client, transcript, pickers, dialogs
      wm/                    tiling window manager: workspaces, floats, bar, palette
      cards/                 window contents: composer, list, form, sheet, terminal
      arena/                 3D crew floor driven by agent events
      vxl/                   voxel renderer to terminal cells
      vt/                    terminal emulator for shell windows
      keys/                  key map and chords
      theme/                 palettes and glyph sets
      gfx/                   inline images: kitty, sixel
    update/                  release channels, download, checksum, binary swap, gateway restart
    vault/                   encrypted autofill vault and OS keychain access
  sdk/
    plugin/                  public Go SDK for plugins built as WebAssembly modules or programs
  desktop/                   desktop app, its own Go module on Wails
    cmd/
      icebath-desktop/       shell entry: windows, menu, tray, deep links, service registration
      icebath-setup/         graphical installer entry
    internal/
      backend/               starts, owns, probes, recycles the local gateway
      chrome/                context menu, find in page, spellcheck
      connection/            connection registry, profiles, cloud and SSH remotes, OAuth, REST proxy
      files/                 dialogs, directory reads, watches, plugin file grants
      git/                   worktrees, status, diffs, review, pull requests
      lifecycle/             deep links and single-instance handoff
      overlay/               HUD, pet overlay, quick entry, wake indicator
      preview/               preview and session windows, link metadata, external open
      storage/               preferences, pastes, downloads, media protocol
      system/                clipboard, notifications, power, screenshots, microphone
      terminal/              PTY sessions and external terminal launch
      tray/                  tray icon and minimize to tray
      updater/               bootstrap, update check and apply, holds, uninstall
      window/                window state, zoom, translucency, menus
    build/                   Wails packaging: icons, installers, signing
  ui/                        the only TypeScript in the repository, browser code built by Vite
    desktop/                 React renderer for the desktop app, also built as the browser webapp
    dashboard/               React dashboard embedded in the binary
    setup/                   installer screens
    shared/                  code shared by the three: RPC client, i18n, generated gateway contract
    bridge/                  generated bindings from the renderer to the Wails services
  skills/
    bundled/                 skills installed by default: Markdown, templates, assets
    optional/                skills installable on request
  locales/                   translation catalogs embedded in the binary
  catalog/
    mcp/                     optional MCP server manifests
    plugins/                 installable plugin index
  tools/                     developer and release programs, never shipped
    brand/                   renders icons from assets/
    contractgen/             writes the gateway contract under ui/shared from the Go contracts
    glyphgen/                writes the voxel glyph tables
    pin/                     updates the pinned versions in the toolchain lock
    pypiwheels/              wraps release binaries as wheels for the pipx channel
    release/                 publishes channel records and release pointers
  third_party/
    anydoc/                  Rust shim and build recipe for the document converter module
  packaging/                 package manager stamps and the Chocolatey package
  nix/                       Nix package, modules, dev shell
  docker/                    files copied into the container image
  site/                      icebath.app worker, install scripts, signing keys
  assets/                    brand images
  .github/workflows/         release, site, winget
```

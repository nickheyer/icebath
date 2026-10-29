<div align="center">

<img src="./assets/ICE_BATH_BANNER_ART.png" alt="icebath" width="100%" />

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./assets/ICE_BATH_BANNER_TEXT_WHITE.png" />
  <source media="(prefers-color-scheme: light)" srcset="./assets/ICE_BATH_BANNER_TEXT_BLACK.png" />
  <img src="./assets/ICE_BATH_BANNER_TEXT_WHITE.png" alt="icebath" width="520" />
</picture>

<br />

<a href="https://icebath.app"><img src="https://img.shields.io/badge/site-icebath.app-0b0f14?style=for-the-badge&labelColor=0b0f14&color=1c2733" alt="icebath.app" /></a>
<img src="https://img.shields.io/badge/go-1.27-0b0f14?style=for-the-badge&logo=go&logoColor=dfe9f2&labelColor=0b0f14&color=1c2733" alt="go 1.27" />
<img src="https://img.shields.io/badge/linux-amd64-0b0f14?style=for-the-badge&logo=linux&logoColor=dfe9f2&labelColor=0b0f14&color=1c2733" alt="linux amd64" />

<br />
<br />

<samp>A tiling agentic harness and netrunner toolkit for a deep dive beyond the blackwall.</samp>

<br />
<br />

<kbd>&nbsp;<a href="#-install">INSTALL</a>&nbsp;</kbd>&nbsp;&nbsp;<kbd>&nbsp;<a href="#-usage">USAGE</a>&nbsp;</kbd>&nbsp;&nbsp;<kbd>&nbsp;<a href="#-configuration">CONFIG</a>&nbsp;</kbd>

</div>

<br />

<p align="center">
<code>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</code>
</p>

## ▍ Install

```sh
curl -fsSL icebath.app | sh
irm icebath.app | iex
brew install nickheyer/tap/icebath
yay -S icebath-bin
winget install nickheyer.icebath
scoop bucket add nickheyer https://github.com/nickheyer/scoop-bucket && scoop install nickheyer/icebath
choco install icebath
pipx install icebath-cli
nix profile add github:nickheyer/nixpkg#icebath
mise use -g github:nickheyer/icebath
docker run -it --rm -v "$PWD:/work" ghcr.io/nickheyer/icebath
go install github.com/nickheyer/icebath/cmd/icebath@latest
# apt, dnf and apk https://icebath.app
```

<br />

## ▍ Usage

```sh
# Just run it and set things up in the TUI
icebath
```

```
# All the flags and modes of operation
icebath [flags] [prompt]
  -profile name      endpoint profile: anthropic, openai, openai-chat, google, mistral, openrouter, ollama, bedrock, nebu
  -model id          model the profile serves
  -mode name         default, acceptEdits, plan or bypassPermissions
  -resume id         continue a stored session
  -c, -continue      continue the latest session of this project
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
```

```sh
# Use icebath as a cli tool if need be
export ANTHROPIC_API_KEY=...
icebath "check disk usage"
echo "summarize go.mod" | icebath -print -yes
icebath -mission "map the open ports on 10.0.0.0/24"
icebath -workflow loop "make every test pass"
icebath -kind critic "review pkg/agent"
```

<br />

## ▍ Configuration

<table align="center">
  <tr>
    <th align="left">Configuration Files</th>
  </tr>
  <tr>
    <td><code>~/.config/icebath/config.json</code> or <code>.icebath/config.json</code></td>
  </tr>
  <tr>
    <td><code>rules.json</code></td>
  </tr>
  <tr>
    <td><code>hooks.json</code></td>
  </tr>
  <tr>
    <td><code>kinds/*.json</code></td>
  </tr>
  <tr>
    <td><code>~/.config/icebath/profiles.json</code></td>
  </tr>
  <tr>
    <td><code>~/.config/icebath/keybinds.json</code></td>
  </tr>
</table>

<br />

<p align="center">
<code>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</code>
</p>

<div align="center">
<sub><samp>▍ Need a runtime manager and gateway? See <a href="https://github.com/nickheyer/nebu">nebu</a> ▍</samp></sub>
</div>

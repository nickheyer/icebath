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

<kbd>&nbsp;<a href="#-install">INSTALL</a>&nbsp;</kbd>&nbsp;&nbsp;<kbd>&nbsp;<a href="#-usage">USAGE</a>&nbsp;</kbd>

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
nix profile add github:nickheyer/nixpkg#icebath
mise use -g github:nickheyer/icebath
pipx install icebath-cli
docker run -it --rm -e ICEBATH_UID=$(id -u) -e ICEBATH_GID=$(id -g) -v ~/.icebath:/opt/data ghcr.io/nickheyer/icebath
go install -tags goolm github.com/nickheyer/icebath/cmd/icebath@latest
git clone https://github.com/nickheyer/icebath && icebath/setup-icebath.sh
# apt, dnf and apk https://icebath.app
```

## ▍ Usage

```sh
icebath                         # open the TUI, set up a model there on first run
icebath chat -q "check disk usage"
icebath gateway install         # messaging gateway as a background service
icebath update
icebath uninstall
```

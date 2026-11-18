<p align="center"><img src="../assets/spotify.jpg" alt="Spotify: official Icebath Agent plugin" width="100%"></p>

# Spotify for Icebath Agent

> Compiled into icebath; `icebath plugins install spotify` publishes it into a profile.

icebath plugin for
[Spotify](https://www.spotify.com/). It gives the agent seven tools over the Spotify Web API and adds
a `icebath spotify` command for logging in:

| Tool | What it does |
|---|---|
| `spotify_playback` | Play, pause, skip, seek, shuffle, repeat, volume, current state, recently played |
| `spotify_devices` | List Spotify Connect devices, transfer playback |
| `spotify_queue` | Read the queue, add to it |
| `spotify_search` | Search tracks, albums, artists, playlists, shows, episodes |
| `spotify_playlists` | List, read, create, edit playlists; add and remove items |
| `spotify_albums` | Album metadata and track lists |
| `spotify_library` | Saved tracks and albums: list, save, remove |

This used to be built into Icebath core. It now ships as a standalone plugin with the same toolset
(`spotify`), tool names and schemas, env vars and stored login. **If you already used Spotify with
Icebath, you don't need to do anything:** `icebath update` (or the first start after updating) installs
and enables this plugin for you and your login keeps working. The one change is the login command:
`icebath auth spotify` is now `icebath spotify login`.

## Install

From the Icebath plugin catalog:

```bash
icebath plugins install spotify
icebath tools enable spotify      # the toolset is opt-in
icebath spotify login
```

Plugins are installed per profile: use `icebath -p <profile> plugins install spotify` for another
profile. The plugin is compiled into icebath, so there is nothing else to install.

## Logging in

Spotify requires every user to register their own lightweight developer app (Spotify does not allow
a shared public client). `icebath spotify login` walks you through it the first time:

1. It opens <https://developer.spotify.com/dashboard>. Click **Create app** and use
   `http://127.0.0.1:43827/spotify/callback` as the **Redirect URI**, with **Web API** checked.
   HTTP is permitted on a loopback IP literal (Spotify's [redirect URI
   rules](https://developer.spotify.com/documentation/web-api/concepts/redirect_uri) only require
   HTTPS for everything else). If your dashboard refuses the `http://` form, register
   `https://127.0.0.1:43827/spotify/callback` instead and set that URI with
   `ICEBATH_SPOTIFY_REDIRECT_URI`: the listener then serves **https with a self-signed
   certificate**, so the browser shows the usual "not private" warning once — accept it and
   continue, the connection never leaves your machine. Paste the URI into the dashboard without
   a trailing space: the field rejects that with "This redirect URI is not secure", which reads
   like a policy problem rather than a stray character.
2. Paste the app's **Client ID** at the prompt. It is saved as `ICEBATH_SPOTIFY_CLIENT_ID` in the
   profile's `.env`, so later logins skip this step.
3. Approve access in the browser. Tokens are stored under `providers.spotify` in the profile's
   `auth.json` and refreshed automatically.

```bash
icebath spotify login [--client-id ID] [--redirect-uri URI] [--scope "..."] [--no-browser] [--timeout S]
icebath spotify status
icebath spotify logout
```

Over SSH, forward the callback port first (`ssh -N -L 43827:127.0.0.1:43827 user@host`) and run
`icebath spotify login --no-browser`, then open the printed authorization URL in a local browser.

| Variable | Purpose |
|---|---|
| `ICEBATH_SPOTIFY_CLIENT_ID` | Your Spotify app's Client ID (the login wizard sets it) |
| `ICEBATH_SPOTIFY_REDIRECT_URI` | Redirect URI, if you registered something other than the default |

## Free vs Premium

Search, playlists, albums, library, queue reads, device lists and playback state work on a Free
account. Playback control (play, pause, skip, seek, volume, shuffle/repeat, queue add, transfer)
needs Premium and an active Spotify Connect device: open Spotify on any phone, desktop or speaker.

Full reference: `icebath spotify --help`.

## License

MIT, see LICENSE and THIRD_PARTY_NOTICES of icebath. Originally contributed by @dlkakbs.

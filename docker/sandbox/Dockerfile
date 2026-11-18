# Icebath sandbox with Python, Node, an Xfce desktop over TigerVNC, Playwright Chromium and cua-driver
FROM python:3.14-slim-trixie@sha256:9aaa474097d094040649f2162a627c0049b408f52138d980916788d80f04802f
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8 PLAYWRIGHT_BROWSERS_PATH=/opt/playwright
RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates curl git openssh-client less nano procps unzip xz-utils build-essential \
        tigervnc-standalone-server xauth x11-utils x11-xserver-utils x11-xkb-utils dbus dbus-x11 \
        xfwm4 xfce4-panel xfdesktop4 xfce4-settings xfconf xfce4-terminal thunar mousepad \
        adwaita-icon-theme gnome-themes-extra fonts-dejavu-core fonts-liberation fonts-noto-color-emoji \
        libnss3 libatk1.0-0t64 libatk-bridge2.0-0t64 libcups2t64 libdrm2 libxkbcommon0 libxcomposite1 \
        libxdamage1 libxfixes3 libxrandr2 libgbm1 libasound2t64 libpango-1.0-0 libcairo2 libxshmfence1 \
        libgtk-3-0t64 libxtst6 libxdo3 libxcb1 \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; arch=$(dpkg --print-architecture); \
    case "$arch" in \
        amd64) node=node-v26.12.1-linux-x64; node_sha=6170b2d474e1c626f43cff3adc118df9fe3b573258a4fc3d850c8dbc6b079bbd; \
               chrome_url=https://cdn.playwright.dev/builds/cft/145.0.7632.6/linux64/chrome-linux64.zip; chrome_sha=b5e3195041af345a668d110f5daf5581961fa3608626ea588c97dd0fe81c4e38; \
               cua=cua-driver-rs-0.21.0-linux-x86_64-binary; cua_sha=cf29bfaad16737e31e72f38dad2a6424f24b21a4ac2edea885e83cb9d5034135 ;; \
        arm64) node=node-v26.12.1-linux-arm64; node_sha=42aa04922591ee5d4dbc7740d452621acf28508239daa512f5ee8c47ee224fdb; \
               chrome_url=https://cdn.playwright.dev/dbazure/download/playwright/builds/chromium/1208/chromium-linux-arm64.zip; chrome_sha=70b4620521b1ada360475e1059dfe9d549a6461a79b9342f85b7d47e44123077; \
               cua=cua-driver-rs-0.21.0-linux-arm64-binary; cua_sha=6b3a308c6741dd8291aad22a308bd2ec8eb52eab64687be007ac86ceaf7e3307 ;; \
        *) echo "no sandbox build for $arch" >&2; exit 1 ;; \
    esac; \
    tmp=$(mktemp -d); cd "$tmp"; \
    curl -fsSLo node.tar.xz "https://nodejs.org/dist/v26.12.1/$node.tar.xz"; \
    echo "$node_sha  node.tar.xz" | sha256sum -c -; \
    tar -xJf node.tar.xz -C /usr/local --strip-components=1 --exclude=CHANGELOG.md --exclude=LICENSE --exclude=README.md; \
    curl -fsSLo chrome.zip "$chrome_url"; \
    echo "$chrome_sha  chrome.zip" | sha256sum -c -; \
    mkdir -p /opt/playwright/chromium-1208; unzip -q chrome.zip -d /opt/playwright/chromium-1208; \
    curl -fsSLo cua.tar.gz "https://github.com/trycua/cua/releases/download/cua-driver-rs-v0.21.0/$cua.tar.gz"; \
    echo "$cua_sha  cua.tar.gz" | sha256sum -c -; \
    mkdir cua; tar -xzf cua.tar.gz -C cua; \
    install -m 0755 "$(find cua -type f -name cua-driver | head -n1)" /usr/local/bin/cua-driver; \
    cd /; rm -rf "$tmp"; \
    chmod -R a+rX /opt/playwright
RUN pip install --no-cache-dir pillow==13.0.0
RUN useradd --create-home --uid 1000 --shell /bin/bash pn
# Cua programs start with their maker's product telemetry and release check off
ENV CUA_DRIVER_RS_TELEMETRY_ENABLED=false CUA_TELEMETRY_ENABLED=false CUA_TELEMETRY=0 DO_NOT_TRACK=1 CUA_DRIVER_RS_UPDATE_CHECK=false
# Chromium policy that keeps the browser from calling Google services on its own
RUN set -eu; \
    policy='{"AIModeSettings":1,"AlternateErrorPagesEnabled":false,"BrowserNetworkTimeQueriesEnabled":false,"BrowserSignin":0,"ChromeVariations":2,"ComponentUpdatesEnabled":false,"DefaultBrowserSettingEnabled":false,"DomainReliabilityAllowed":false,"EnableMediaRouter":false,"GenAILocalFoundationalModelSettings":1,"MetricsReportingEnabled":false,"NetworkPredictionOptions":2,"NewTabPageLocation":"about:blank","PasswordLeakDetectionEnabled":false,"PromotionsEnabled":false,"SafeBrowsingProtectionLevel":0,"SearchSuggestEnabled":false,"SpellCheckServiceEnabled":false,"SpellcheckEnabled":false,"SyncDisabled":true,"TranslateEnabled":false,"UrlKeyedAnonymizedDataCollectionEnabled":false}'; \
    for d in /etc/opt/chrome_for_testing/policies/managed /etc/chromium/policies/managed; do \
        mkdir -p "$d"; printf '%s\n' "$policy" > "$d/icebath.json"; \
    done

# syntax=docker/dockerfile:1.7
# BINARY_SOURCE=source builds icebath from this checkout, release takes the
# goreleaser binary from <context>/<os>/<arch>/icebath.
ARG BINARY_SOURCE=source
ARG GO_IMAGE=golang:1.27-trixie
ARG NODE_IMAGE=node:26-trixie-slim
ARG RUNTIME_IMAGE=debian:trixie-slim

FROM ${NODE_IMAGE} AS node

FROM ${GO_IMAGE} AS build-source
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.npm \
    set -eu; \
    go run ./cmd/icebath-build web; \
    CGO_ENABLED=0 go build -tags webdist -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/icebath ./cmd/icebath; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/icebath-init ./cmd/icebath-init

FROM scratch AS build-release
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/icebath ${TARGETPLATFORM}/icebath-init /out/

FROM build-${BINARY_SOURCE} AS binary

FROM ${RUNTIME_IMAGE} AS runtime
# Tools the agent drives from its terminal and browser tools, the shadow
# utilities the boot remap uses, and the shared libraries pm's Chromium needs.
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
    ca-certificates curl iputils-ping procps git openssh-client docker-cli xz-utils tmux passwd tzdata \
    libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatspi2.0-0t64 libcairo2 libcups2t64 libdbus-1-3 libgbm1 \
    libglib2.0-0t64 libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 \
    libxkbcommon0 libxrandr2 && \
    rm -rf /var/lib/apt/lists/*

# Optional desktop for the bot desktop tools (VNC, Xfce, distribution Chromium).
ARG ICEBATH_BOT_DESKTOP=0
RUN if [ "$ICEBATH_BOT_DESKTOP" = "1" ]; then \
        apt-get -o Acquire::Retries=3 update && \
        DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        tigervnc-standalone-server xfce4-panel xfwm4 xfdesktop4 xfce4-settings xfce4-terminal \
        dbus-x11 x11-xserver-utils x11-utils x11-xkb-utils xauth fonts-dejavu-core chromium && \
        rm -rf /var/lib/apt/lists/*; \
    fi

RUN useradd -u 10000 -m -d /opt/data icebath && \
    mkdir -p /run/s6 /run/service && chown 10000:10000 /run/s6 /run/service
WORKDIR /opt/icebath
COPY --from=binary --chmod=0755 /out/icebath /out/icebath-init /opt/icebath/
COPY --chmod=0644 LICENSE README.md cli-config.yaml.example .env.example /opt/icebath/
COPY --chmod=0644 docker/SOUL.md /opt/icebath/docker/SOUL.md

# icebath-init is the init, supervisor, s6 control verbs and tini shim under those names, handing icebath work to the binary beside it
RUN set -eu; \
    mkdir -p /command; \
    ln -s /opt/icebath/icebath-init /init; \
    ln -s /opt/icebath/icebath-init /usr/bin/tini; \
    for verb in s6-svscan s6-svc s6-svstat s6-svwait s6-svscanctl s6-setuidgid with-contenv s6-log; do \
        ln -s /opt/icebath/icebath-init "/command/$verb"; \
        ln -s "/command/$verb" "/usr/bin/$verb"; \
    done

# Chromium, ffmpeg and ripgrep come from pm's sha-verified lock into the
# read-only tool store; image-tools links them onto PATH and pins Chromium.
ENV ICEBATH_RUNTIME_DIR=/opt/icebath/tools
RUN set -eu; \
    ICEBATH_HOME=/tmp/icebath-build-home /opt/icebath/icebath pm install chromium ffmpeg ripgrep; \
    ICEBATH_HOME=/tmp/icebath-build-home /opt/icebath/icebath container-stage image-tools; \
    ffmpeg -version >/dev/null; ffprobe -version >/dev/null; rg --version >/dev/null; \
    "$(cat /etc/icebath/chromium-executable-path)" --version; \
    rm -rf /tmp/icebath-build-home; \
    chmod -R a+rX,go-w /opt/icebath

# Install method and provenance stamps live beside the code, never in the
# shared /opt/data volume. The image is rebuilt and re-pulled, never updated
# in place, so the stamp's update mechanism is external.
ARG VERSION=dev
ARG REVISION=
RUN set -eu; \
    printf 'docker\n' > /opt/icebath/.install_method; \
    if [ ! -f /opt/icebath/install-stamp.json ]; then \
        commit="${REVISION:-0000000000000000000000000000000000000000}"; \
        printf '{"schemaVersion":2,"commit":"%s","distribution":"docker","source":"%s","updateMechanism":"external"}\n' \
            "$commit" "$([ -n "$REVISION" ] && echo git || echo fallback)" > /opt/icebath/install-stamp.json; \
    fi; \
    mkdir -p /etc/icebath; \
    revision=null; [ -n "$REVISION" ] && revision="\"$REVISION\""; \
    printf '{"deployment_kind":"image","image":"ghcr.io/nickheyer/icebath","manager":"docker","revision":%s,"schema":1,"version":"%s"}\n' \
        "$revision" "$VERSION" > /etc/icebath/image-provenance.json; \
    chmod 0444 /etc/icebath/image-provenance.json; \
    mkdir -p /opt/data

LABEL org.opencontainers.image.source="https://github.com/nickheyer/icebath" \
      org.opencontainers.image.description="Tiling agentic harness and netrunner toolkit" \
      org.opencontainers.image.licenses="MIT"

ENV ICEBATH_HOME=/opt/data
ENV ICEBATH_WRITE_SAFE_ROOT=/opt/data
# Container scoped runtime dir for Xfce, dbus and the display lock, seeded 0700 at boot.
ENV XDG_RUNTIME_DIR=/tmp/icebath-runtime
ENV PATH="/opt/icebath:/command:/opt/data/.local/bin:${PATH}"
# Login shells re-read /etc/profile, which resets PATH; this re-adds the dirs above.
COPY --chmod=0644 docker/profile-icebath-path.sh /etc/profile.d/icebath.sh
VOLUME [ "/opt/data" ]
EXPOSE 9119
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD [ "/opt/icebath/icebath-init", "container-health" ]

# /init bootstraps the volume as root (UID/GID remap, ownership, seeding),
# supervises the dashboard and per-profile gateway services, then runs the
# command as the icebath user and exits with its status:
#   docker run <image>                 -> icebath
#   docker run <image> gateway run     -> icebath gateway run
#   docker run <image> sleep infinity  -> sleep infinity
#   docker run <image> --version       -> icebath --version
ENTRYPOINT [ "/init" ]
CMD [ ]

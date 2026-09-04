# syntax=docker/dockerfile:1
#
# Railway build for vpn-ui (container profile).
#
# This image carries ONLY the parts of vpn-ui that can work inside an
# unprivileged container:
#
#   - the web panel (management UI, API, multi-admin, reseller, Telegram bot)
#   - the pinned, patched Xray core (built from the exact submodule commit the
#     repo pins) and the base geo files, both baked into the binary via go:embed
#   - every Xray-core protocol: VLESS, VMess, Trojan, Shadowsocks (incl.
#     AES-GCM), socks/http, XHTTP, plus the fork's native AnyTLS, TUIC v5 and
#     NaiveProxy
#
# It deliberately does NOT ship the kernel-protocol daemon bundle (PPTP, L2TP,
# IPsec, OpenVPN, OpenConnect, SSTP, IKEv2, kernel WireGuard, AmneziaWG, GRE):
# those need kernel modules, TUN devices and NET_ADMIN, none of which a
# container platform provides. The panel simply shows those cores as not
# installed. Everything else works.
#
# Build stages:
#   xray-core  — gets the pinned Xray core (the commit pinned in .gitmodules).
#                Preferred path: download the fork's RELEASE BINARY for the
#                exact pinned tag (fast, light — fits Railway's builders).
#                Fallback: compile from the cloned pin when the pin has no
#                exact tag or XRAY_BUILD_MODE=source. Railway does not init
#                git submodules, so the build fetches the pin itself either way
#   geofiles   — downloads the base geoip.dat / geosite.dat pair (Loyalsoldier).
#                Country files (IR/RU) are opt-in build args; the panel also
#                downloads one on demand the first time a routing rule needs it
#   panel      — compiles the vpn-ui binary with the core + geo files embedded
#   runtime    — debian:bookworm-slim + ca-certificates + tzdata, runs as root
#                (the panel requires root), wired for Railway: 0.0.0.0:$PORT,
#                state under /data
#
# The entrypoint (deploy/railway/entrypoint.sh) syncs the panel port to
# Railway's $PORT, provisions first-run admin credentials from env vars and
# execs the server.

ARG GO_VERSION=1.26
ARG XRAY_REPO=https://github.com/Sir-MmD/Xray-core
ARG XRAY_COMMIT=bacaa47268398bbeeddc9c3c88e1ee143101da21

# ---------------------------------------------------------------------------
# Stage 1: the pinned patched Xray core
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS xray-core

ARG XRAY_REPO
ARG XRAY_COMMIT
ARG TARGETARCH=amd64
# auto (default) = release binary of the pinned tag, else compile from source.
# source = always compile. prebuilt = only the release binary (fails if absent).
ARG XRAY_BUILD_MODE=auto

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip jq git \
    && rm -rf /var/lib/apt/lists/*

# The repo's submodule pin (.gitmodules) is Sir-MmD/Xray-core @ XRAY_COMMIT.
RUN git clone --filter=blob:none "${XRAY_REPO}" /xray-src \
    && git -C /xray-src checkout --detach "${XRAY_COMMIT}"

WORKDIR /xray-src

# 1) Preferred: the fork's release CI builds and publishes binaries per tag —
#    download the one for the exact pinned tag (name per the fork's own
#    .github/build/friendly-filenames.json). ~30 seconds, no compile.
# 2) Fallback: compile from the pinned checkout with the same flags the fork's
#    release CI uses. GOFLAGS caps Go's parallelism so the compile also fits
#    memory-limited builders (Railway Hobby) at the cost of some speed.
#
# NOTE: no RUN --mount=type=cache here. Railway's Dockerfile parser rejects
# cache mounts without an explicit `id=` (plain BuildKit accepts them), and
# Railway does not keep the cache between builds anyway.
RUN set -eux; \
    mkdir -p /out; \
    if [ "${XRAY_BUILD_MODE}" != "source" ]; then \
        tag="$(git -C /xray-src describe --tags --exact-match "${XRAY_COMMIT}" 2>/dev/null || true)"; \
        case "${TARGETARCH}" in \
            arm64) jkey="linux-arm64-v8a" ;; \
            *)     jkey="linux-amd64" ;; \
        esac; \
        if [ -n "${tag}" ]; then \
            fkey="$(jq -r --arg k "${jkey}" '.[$k].friendlyName' /xray-src/.github/build/friendly-filenames.json 2>/dev/null || true)"; \
            slug="${XRAY_REPO#https://github.com/}"; slug="${slug%.git}"; \
            asset="$(curl -fsSL --retry 3 --retry-all-errors \
                "https://api.github.com/repos/${slug}/releases/tags/${tag}" \
                | jq -r --arg f "${fkey}" \
                    '[.assets[]? | select(.name == $f)][0].browser_download_url // empty' 2>/dev/null || true)"; \
            if [ -n "${asset}" ]; then \
                echo "using prebuilt release: ${asset}"; \
                if curl -fsSL --retry 3 --retry-all-errors -o /tmp/xray.zip "${asset}" \
                    && unzip -o /tmp/xray.zip -d /tmp/xrayzip >/dev/null \
                    && find /tmp/xrayzip -type f -name xray -exec cp {} /out/xray \; \
                    && chmod 0755 /out/xray \
                    && /out/xray version; then \
                    echo "prebuilt core OK"; \
                else \
                    echo "prebuilt download unusable — falling back to source compile"; \
                    rm -f /out/xray; \
                fi; \
            else \
                echo "no release asset '${fkey}' for tag '${tag}'"; \
            fi; \
        else \
            echo "pinned commit ${XRAY_COMMIT} has no exact tag — will compile"; \
        fi; \
    fi; \
    if [ ! -s /out/xray ]; then \
        if [ "${XRAY_BUILD_MODE}" = "prebuilt" ]; then \
            echo "XRAY_BUILD_MODE=prebuilt but no release binary found" >&2; \
            exit 1; \
        fi; \
        echo "compiling the pinned Xray core from source"; \
        GOFLAGS="-p=2" CGO_ENABLED=0 go build -o /out/xray -trimpath -buildvcs=false \
            -gcflags="all=-l=4" \
            -ldflags="-s -w -buildid= -X github.com/xtls/xray-core/core.build=${XRAY_COMMIT}" \
            ./main; \
    fi; \
    test -x /out/xray

# ---------------------------------------------------------------------------
# Stage 2: base geo files
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS geofiles

# Optional country geo files baked into the binary. Default off (GEO_LEAN
# profile, ~118MB smaller image): the panel downloads one automatically the
# first time a routing rule names it. Set GEO_IR=1 / GEO_RU=1 as build args
# (or in the template's build args) to bake them in.
ARG GEO_IR=0
ARG GEO_RU=0

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Same sources build/core/build.sh uses (and the dashboard's own updater).
# --retry-all-errors: GitHub's CDN answers transient 403/429 under load; a bare
# -f failure here must not fail the whole Railway build.
RUN set -eux; \
    mkdir -p /out; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /out/geoip.dat \
      "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /out/geosite.dat \
      "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"; \
    if [ "${GEO_IR}" = "1" ]; then \
      curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /out/geoip_IR.dat \
        "https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download/geoip.dat"; \
      curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /out/geosite_IR.dat \
        "https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download/geosite.dat"; \
    fi; \
    if [ "${GEO_RU}" = "1" ]; then \
      curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /out/geoip_RU.dat \
        "https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download/geoip.dat"; \
      curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /out/geosite_RU.dat \
        "https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download/geosite.dat"; \
    fi

# ---------------------------------------------------------------------------
# Stage 3: the vpn-ui panel binary
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS panel

# BuildKit sets TARGETARCH from the build platform (amd64 on Railway's
# builders). Falls back to amd64 for plain `docker build` without BuildKit.
ARG TARGETARCH=amd64

WORKDIR /src

# .dockerignore keeps the context small; everything the go:embed directives
# need (web assets, awgsrc module sources, sub templates, ...) is tracked in
# git and copied with the rest.
COPY . .

# Bake the pinned Xray core and geo files into the binary, exactly like
# ./build.sh does for a release (corebundle/core/<arch>/xray + corebundle/core/*.dat).
COPY --from=xray-core /out/xray corebundle/core/${TARGETARCH}/xray
COPY --from=geofiles /out/ corebundle/core/

# Cross-compile support: CGO is required (SQLite). The golang image ships a
# host gcc; arm64 needs a cross toolchain. (Railway's builders are amd64 —
# this only matters for local/dockerx arm builds.)
RUN if [ "${TARGETARCH}" = "arm64" ]; then \
        apt-get update \
        && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross \
        && rm -rf /var/lib/apt/lists/*; \
    fi

# Same compile build.sh runs: CGO_ENABLED=1, single main.go entry file.
# GOFLAGS="-p=2" caps Go's compile parallelism so this also fits
# memory-limited builders (Railway Hobby) — a slower, much lighter build.
# (No cache mounts: Railway's parser rejects them without `id=`, and the cache
# does not persist between Railway builds anyway.)
RUN if [ "${TARGETARCH}" = "arm64" ]; then \
        CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOARCH=arm64 GOFLAGS="-p=2" go build -o /out/vpn-ui main.go; \
    else \
        CGO_ENABLED=1 GOARCH=amd64 GOFLAGS="-p=2" go build -o /out/vpn-ui main.go; \
    fi

# ---------------------------------------------------------------------------
# Stage 4: runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=panel /out/vpn-ui /app/vpn-ui
COPY deploy/railway/entrypoint.sh /app/entrypoint.sh
RUN chmod 0755 /app/vpn-ui /app/entrypoint.sh \
    && mkdir -p /data

# The panel requires root (it supervises processes and writes its state
# folders); Railway containers run as root by default.
# PORT falls back to 2083 for local docker runs; Railway overrides it with the
# service's real target port and the entrypoint keeps the panel in sync.
ENV PORT=2083 \
    VPNUI_DB_FOLDER=/data \
    VPNUI_LOG_FOLDER=/data/logs \
    VPNUI_BIN_FOLDER=/data/bin \
    NO_COLOR=1

EXPOSE 2083

ENTRYPOINT ["/app/entrypoint.sh"]ARG GO_VERSION=1.26
ARG XRAY_REPO=https://github.com/Sir-MmD/Xray-core
ARG XRAY_COMMIT=bacaa47268398bbeeddc9c3c88e1ee143101da21

# ---------------------------------------------------------------------------
# Stage 1: the pinned patched Xray core
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS xray-core

ARG XRAY_REPO
ARG XRAY_COMMIT

# The repo's submodule pin (.gitmodules) is Sir-MmD/Xray-core @ XRAY_COMMIT.
# Railway does not initialise submodules during builds, so the Docker build
# fetches the exact pinned commit itself.
RUN git clone --filter=blob:none "${XRAY_REPO}" /xray-src \
    && git -C /xray-src checkout --detach "${XRAY_COMMIT}"

WORKDIR /xray-src

# Same build the fork's own .github/workflows/release.yml runs for linux
# (CGO disabled, same trimpath/gcflags/ldflags), so the core behaves exactly
# like a release binary. Stamp core.build with the pinned commit.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/xray -trimpath -buildvcs=false \
        -gcflags="all=-l=4" \
        -ldflags="-s -w -buildid= -X github.com/xtls/xray-core/core.build=${XRAY_COMMIT}" \
        ./main

# ---------------------------------------------------------------------------
# Stage 2: base geo files
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS geofiles

# Optional country geo files baked into the binary. Default off (GEO_LEAN
# profile, ~118MB smaller image): the panel downloads one automatically the
# first time a routing rule names it. Set GEO_IR=1 / GEO_RU=1 as build args
# (or in the template's build args) to bake them in.
ARG GEO_IR=0
ARG GEO_RU=0

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Same sources build/core/build.sh uses (and the dashboard's own updater).
RUN set -eux; \
    mkdir -p /out; \
    curl -fsSL --retry 3 -o /out/geoip.dat \
      "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"; \
    curl -fsSL --retry 3 -o /out/geosite.dat \
      "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"; \
    if [ "${GEO_IR}" = "1" ]; then \
      curl -fsSL --retry 3 -o /out/geoip_IR.dat \
        "https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download/geoip.dat"; \
      curl -fsSL --retry 3 -o /out/geosite_IR.dat \
        "https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download/geosite.dat"; \
    fi; \
    if [ "${GEO_RU}" = "1" ]; then \
      curl -fsSL --retry 3 -o /out/geoip_RU.dat \
        "https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download/geoip.dat"; \
      curl -fsSL --retry 3 -o /out/geosite_RU.dat \
        "https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download/geosite.dat"; \
    fi

# ---------------------------------------------------------------------------
# Stage 3: the vpn-ui panel binary
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS panel

# BuildKit sets TARGETARCH from the build platform (amd64 on Railway's
# builders). Falls back to amd64 for plain `docker build` without BuildKit.
ARG TARGETARCH=amd64

WORKDIR /src

# .dockerignore keeps the context small; everything the go:embed directives
# need (web assets, awgsrc module sources, sub templates, ...) is tracked in
# git and copied with the rest.
COPY . .

# Bake the pinned Xray core and geo files into the binary, exactly like
# ./build.sh does for a release (corebundle/core/<arch>/xray + corebundle/core/*.dat).
COPY --from=xray-core /out/xray corebundle/core/${TARGETARCH}/xray
COPY --from=geofiles /out/ corebundle/core/

# Cross-compile support: CGO is required (SQLite). The golang image ships a
# host gcc; arm64 needs a cross toolchain. (Railway's builders are amd64 —
# this only matters for local/dockerx arm builds.)
RUN if [ "${TARGETARCH}" = "arm64" ]; then \
        apt-get update \
        && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross \
        && rm -rf /var/lib/apt/lists/*; \
    fi

# Same compile build.sh runs: CGO_ENABLED=1, single main.go entry file.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "${TARGETARCH}" = "arm64" ]; then \
        CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOARCH=arm64 go build -o /out/vpn-ui main.go; \
    else \
        CGO_ENABLED=1 GOARCH=amd64 go build -o /out/vpn-ui main.go; \
    fi

# ---------------------------------------------------------------------------
# Stage 4: runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=panel /out/vpn-ui /app/vpn-ui
COPY deploy/railway/entrypoint.sh /app/entrypoint.sh
RUN chmod 0755 /app/vpn-ui /app/entrypoint.sh \
    && mkdir -p /data

# The panel requires root (it supervises processes and writes its state
# folders); Railway containers run as root by default.
# PORT falls back to 2083 for local docker runs; Railway overrides it with the
# service's real target port and the entrypoint keeps the panel in sync.
ENV PORT=2083 \
    VPNUI_DB_FOLDER=/data \
    VPNUI_LOG_FOLDER=/data/logs \
    VPNUI_BIN_FOLDER=/data/bin \
    NO_COLOR=1

EXPOSE 2083

ENTRYPOINT ["/app/entrypoint.sh"]

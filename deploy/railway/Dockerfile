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
#   xray-core  — clones Sir-MmD/Xray-core at the commit pinned in .gitmodules
#                and builds it with the same flags the fork's own release CI
#                uses
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

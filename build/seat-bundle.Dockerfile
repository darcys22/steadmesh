# Explicit multi-harness seat image for startup fallback. The three runtime
# pins match the separately maintained adapter images; no CLI is installed at runtime.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o /out/seat-runner ./cmd/seat-runner && \
    go build -ldflags='-s -w' -o /out/steadmesh-tools ./cmd/steadmesh-tools

FROM node:22-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
ARG CODEX_VERSION=0.160.1
ARG CLAUDE_CODE_VERSION=2.1.289
ARG PI_VERSION=1.0.4
ENV NPM_CONFIG_UPDATE_NOTIFIER=false NPM_CONFIG_FUND=false \
    DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    PI_OFFLINE=1 PI_TELEMETRY=0
RUN npm install -g --omit=dev "@openai/codex@${CODEX_VERSION}" "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && \
    npm install -g --omit=dev --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" && \
    npm cache clean --force && \
    test "$(HOME=/tmp codex --version)" = "codex-cli ${CODEX_VERSION}" && \
    v="$(HOME=/tmp claude --version)" && case "$v" in "${CLAUDE_CODE_VERSION} "*) ;; *) exit 1;; esac && \
    v="$(HOME=/tmp pi --version)" && case "$v" in *"${PI_VERSION}"*) ;; *) exit 1;; esac && \
    rm -rf /tmp/* /root/.npm && \
    mkdir -p /seat/workspace /seat/home /seat/runner && chown -R 1000:1000 /seat
# Tools for access profiles (docs/sandbox.html): git, gh and curl from
# Debian; gh runs through a wrapper that fetches the seat's GitHub token.
RUN apt-get update && apt-get install -y --no-install-recommends git gh curl ca-certificates && \
    rm -rf /var/lib/apt/lists/* && git --version && gh --version | head -1 && curl --version | head -1
COPY build/gh-wrapper.sh /usr/local/bin/gh
COPY --from=build /out/seat-runner /out/steadmesh-tools /usr/local/bin/
ENV HOME=/seat/home \
    TMPDIR=/tmp \
    STEADMESH_HARNESS=codex \
    STEADMESH_TOOLS_BIN=/usr/local/bin/steadmesh-tools \
    STEADMESH_TOKEN_FILE=/var/run/steadmesh/token \
    STEADMESH_MANIFEST_DIR=/etc/steadmesh/manifest
# No WORKDIR under /seat: the container runtime would create it inside the
# mounted volume as root. seat-runner creates the seat directories itself.
WORKDIR /
# uid 1000 is the base image's "node" user.
USER 1000:1000
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/seat-runner"]
CMD ["run"]

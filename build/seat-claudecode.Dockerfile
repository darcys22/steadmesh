# Seat image with Claude Code (adapter claude-code). The npm version is pinned
# exactly and must match harnesses/claudecode.PinnedVersion; see
# docs/decisions.html#harness. Read-only rootfs: writes go to /seat and /tmp.
# Builder: pinned Go toolchain. Works with the legacy builder and BuildKit.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o /out/seat-runner ./cmd/seat-runner && \
    go build -ldflags='-s -w' -o /out/steadmesh-tools ./cmd/steadmesh-tools

FROM node:22-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
ARG CLAUDE_CODE_VERSION=2.1.289
# Install as root into the image (read-only at runtime), with no update
# checks, then verify the pinned version.
ENV DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 NPM_CONFIG_UPDATE_NOTIFIER=false NPM_CONFIG_FUND=false
RUN npm install -g --omit=dev "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && \
    npm cache clean --force && \
    v="$(HOME=/tmp claude --version)" && echo "claude: $v" && \
    case "$v" in "${CLAUDE_CODE_VERSION} "*) ;; *) echo "expected ${CLAUDE_CODE_VERSION}" >&2; exit 1;; esac && \
    rm -rf /tmp/* /root/.npm && \
    mkdir -p /seat/workspace /seat/home /seat/runner && chown -R 1000:1000 /seat
COPY --from=build /out/seat-runner /out/steadmesh-tools /usr/local/bin/
ENV HOME=/seat/home \
    TMPDIR=/tmp \
    STEADMESH_HARNESS=claude-code \
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

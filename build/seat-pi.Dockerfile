# Seat image with the Pi coding agent (adapter pi, pi.dev). The npm version is
# pinned exactly and must match harnesses/pi.PinnedVersion
# (harnesses/pi/CONTRACT.md). Pi needs Node >= 22.19.0; its CLI is a
# self-contained bundle, so no install scripts run. Read-only rootfs: writes
# go to /seat and /tmp. Builder: pinned Go toolchain.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o /out/seat-runner ./cmd/seat-runner && \
    go build -ldflags='-s -w' -o /out/steadmesh-tools ./cmd/steadmesh-tools

FROM node:22-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
ARG PI_VERSION=1.0.4
ENV NPM_CONFIG_UPDATE_NOTIFIER=false NPM_CONFIG_FUND=false PI_OFFLINE=1 PI_TELEMETRY=0
RUN node -e 'const [a,b]=process.versions.node.split(".").map(Number); if (a<22||(a===22&&b<19)) { console.error("pi needs node >= 22.19.0, have "+process.versions.node); process.exit(1) }' && \
    npm install -g --omit=dev --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" && \
    npm cache clean --force && \
    v="$(HOME=/tmp pi --version)" && echo "pi: $v" && \
    case "$v" in *"${PI_VERSION}"*) ;; *) echo "expected pi ${PI_VERSION}" >&2; exit 1;; esac && \
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
    STEADMESH_HARNESS=pi \
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

# Test-only image. No credentials, model endpoint or production adapter is added.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/steadmesh-tools ./cmd/steadmesh-tools && \
    go test -c -tags integration -o /out/acp-conformance ./harnesses/acp

FROM node:22-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
WORKDIR /opt/acp-runtime
COPY harnesses/acp/contract/runtime/package.json harnesses/acp/contract/runtime/package-lock.json ./
# Integrity-pinned dependency tree, including exact approved runtime versions.
# Lifecycle scripts are disabled. All downloads happen before network isolation.
RUN npm ci --ignore-scripts --omit=dev --no-audit --no-fund && npm cache clean --force
COPY --from=build /out/steadmesh-tools /out/acp-conformance /usr/local/bin/
RUN mkdir -p /opt/steadmesh/acp-cwd && chmod 0755 /opt/steadmesh /opt/steadmesh/acp-cwd
ENV STEADMESH_ACP_NODE=/usr/local/bin/node \
    STEADMESH_ACP_SCRIPT=/opt/acp-runtime/node_modules/@agentclientprotocol/codex-acp/dist/index.js \
    STEADMESH_ACP_CODEX_BIN=/opt/acp-runtime/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex \
    STEADMESH_TOOLS_BIN=/usr/local/bin/steadmesh-tools \
    STEADMESH_ACP_NEUTRAL_DIR=/opt/steadmesh/acp-cwd \
    HOME=/tmp \
    TMPDIR=/tmp
USER 1000:1000
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/acp-conformance"]
CMD ["-test.v", "-test.run", "^Test(CodexACPProtocolConformance|ExperimentalLauncher)", "-test.timeout", "3m"]

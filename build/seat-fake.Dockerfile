# Seat image with the deterministic fake harness (compiled into seat-runner).
# Read-only rootfs: all writes go to /seat (PVC) and /tmp (emptyDir).
# Builder: pinned Go toolchain. Works with the legacy builder and BuildKit.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o /out/seat-runner ./cmd/seat-runner && \
    go build -ldflags='-s -w' -o /out/steadmesh-tools ./cmd/steadmesh-tools && \
    mkdir -p /out/seat/workspace /out/seat/home /out/seat/runner /out/tmp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/seat-runner /out/steadmesh-tools /usr/local/bin/
COPY --from=build --chown=1000:1000 /out/seat /seat
COPY --from=build --chown=1000:1000 /out/tmp /tmp
ENV HOME=/seat/home \
    STEADMESH_HARNESS=fake \
    STEADMESH_TOOLS_BIN=/usr/local/bin/steadmesh-tools \
    STEADMESH_TOKEN_FILE=/var/run/steadmesh/token \
    STEADMESH_MANIFEST_DIR=/etc/steadmesh/manifest
# No WORKDIR under /seat: the container runtime would create it inside the
# mounted volume as root. seat-runner creates the seat directories itself.
WORKDIR /
USER 1000:1000
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/seat-runner"]
CMD ["run"]

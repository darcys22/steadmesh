# Image for cmd/platform. Build: build/build.sh platform (or docker build -f build/platform.Dockerfile .)
# Builder: pinned Go toolchain. Works with the legacy builder and BuildKit.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o /out/platform ./cmd/platform

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/platform /usr/local/bin/platform
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/platform"]

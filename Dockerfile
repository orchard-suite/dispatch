# The assistant asks Claude through the Ion engine, which on a Claude
# subscription spawns the Claude Code CLI. So the image carries the engine
# binary from its pinned image and the pinned CLI, on a Node base the CLI
# needs, beside the static dispatch binary.
ARG ENGINE_IMAGE=ghcr.io/dsswift/ion/engine:3.7.3
FROM ${ENGINE_IMAGE} AS engine

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION ./
COPY cmd ./cmd
COPY internal ./internal
RUN version=$(cut -d' ' -f1 VERSION) \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$version" -o /out/dispatch ./cmd/dispatch

FROM node:22.23.3-alpine3.24
# The Claude Code CLI, for the engine's claude-code backend. The version is
# pinned so a rebuild never changes the agent underneath the assistant.
ARG CLAUDE_CODE_VERSION=2.1.293
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force
# tini is PID 1: the engine `ion prompt` starts outlives it, and the CLI's
# children are reaped when they end.
RUN apk add --no-cache tini ca-certificates
COPY --from=engine /ion /usr/local/bin/ion
COPY --from=build /out/dispatch /app/dispatch
# /data is the desk's volume; /ion is the engine's working directory, kept
# short because the engine puts Unix sockets under it; /home/dispatch is
# HOME, where the CLI keeps its state, already through its first-run setup.
RUN mkdir -p /data /ion /home/dispatch \
 && echo '{"hasCompletedOnboarding":true}' > /home/dispatch/.claude.json \
 && chown -R 65532:65532 /data /ion /home/dispatch
ENV DATA_DIR=/data ION_BIN=/usr/local/bin/ion ION_DATA_DIR=/ion HOME=/home/dispatch
EXPOSE 8080 8081 8082
USER 65532:65532
ENTRYPOINT ["/sbin/tini", "--", "/app/dispatch"]

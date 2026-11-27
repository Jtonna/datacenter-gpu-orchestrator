# llm-scheduler image. Build from the REPO ROOT on the host machine:
#   docker build -f deploy/Dockerfile -t llm-scheduler:local \
#     --build-arg BASE_IMAGE=<image> \
#     --build-arg VERSION=$(git describe --always --dirty) .
#
# BASE_IMAGE is required (no default): an image that already contains
# llama-server and llama-server-rpc with their CUDA libraries, for example
# the one deploy/rpc-build.Dockerfile builds. Nothing is reinstalled here:
# stage 1 builds the static Go binary, the final stage adds it to the base
# and replaces the entrypoint.

# Keep in step with `go` in go.mod.
ARG GO_VERSION=1.25
ARG BASE_IMAGE

# The base image is a required build argument: this small stage fails the
# build at once, with what to pass, when it is missing.
FROM golang:${GO_VERSION} AS base-image-check
ARG BASE_IMAGE
RUN if [ -z "$BASE_IMAGE" ]; then echo "BASE_IMAGE is required: pass --build-arg BASE_IMAGE=<image>, an image that has llama-server (see README.md, Build the scheduler image)." >&2; exit 1; fi \
    && touch /base-image-ok

FROM golang:${GO_VERSION} AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# NOTE: -X only works on a string VARIABLE. cmd/llm-scheduler must declare
# `var version = "dev"` (not `const`) for the stamp to take effect.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/llm-scheduler ./cmd/llm-scheduler

FROM ${BASE_IMAGE:-scratch}
COPY --from=base-image-check /base-image-ok /tmp/.base-image-ok
USER root
COPY --from=build /out/llm-scheduler /usr/local/bin/llm-scheduler

# The HEALTHCHECK below needs curl or wget. The base is Debian/Ubuntu based
# (the rpc-build step installs packages with apt), but whether either tool is
# present depends on the base. Fail the build here, with a
# clear message, rather than ship a container that is always "unhealthy".
RUN command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 \
    || { echo "base image has neither curl nor wget: see docs/runbook.md, section B" >&2; exit 1; }

# /health is 200 whenever the scheduler is running.
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS -o /dev/null http://127.0.0.1:8080/health \
     || wget -q -O /dev/null http://127.0.0.1:8080/health \
     || exit 1

# Deployment paths (the only settings outside the database). Each is also a flag:
# --listen, --data-dir, --models-dir, --history-dir, --child-log-dir (flags win).
# Everything else is configuration in the database under data-dir, changed through
# the REST API, the web UI or MCP. A first install imports deploy/seed-config.yaml once:
#   docker compose run --rm --no-deps potluck import-config /seed/seed-config.yaml
ENV LLM_SCHEDULER_LISTEN=:8080     LLM_SCHEDULER_DATA_DIR=/var/lib/llm-scheduler/data     LLM_SCHEDULER_MODELS_DIR=/models     LLM_SCHEDULER_HISTORY_DIR=/var/lib/llm-scheduler/history     LLM_SCHEDULER_CHILD_LOG_DIR=/var/log/llm-children

# The base image may set its own CMD; clear it.
CMD []
ENTRYPOINT ["/usr/local/bin/llm-scheduler"]

# llm-scheduler image. Build from the REPO ROOT on the host machine:
#   docker build -f deploy/Dockerfile -t llm-scheduler:local \
#     --build-arg VERSION=$(git describe --always --dirty) .
#
# Stage 1 builds the static Go binary. Stage 2 is the image llama-swap runs in
# today. That base already contains llama-server, llama-server-rpc, sd-server
# and the Whisper server, plus its CUDA 13 libraries, so nothing
# is reinstalled here. Only the binary is added and the entrypoint replaced.

# Keep in step with `go` in go.mod.
ARG GO_VERSION=1.25
ARG BASE_IMAGE=llama-swap-whisper:rpc-330f018

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

FROM ${BASE_IMAGE}
USER root
COPY --from=build /out/llm-scheduler /usr/local/bin/llm-scheduler

# The HEALTHCHECK below needs curl or wget. The base is Debian/Ubuntu based
# (the rpc-build step installs packages with apt), but whether either tool is
# present is not recorded in the llama-swap folder. Fail the build here, with a
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

# The base image sets its own CMD for llama-swap; clear it.
CMD []
ENTRYPOINT ["/usr/local/bin/llm-scheduler"]

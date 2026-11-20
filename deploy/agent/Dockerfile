# Potluck agent node image (docs/agent-contract.md sections 5.4, 6.1, 9).
#
# Keep this file in sync with the copy embedded in potluck-agent-setup
# (the local-build fallback of the --docker path).
#
# - Programs (llama-server, rpc-server, node_exporter, nvidia_gpu_exporter)
#   are NOT baked in. Potluck hands them out at registration; the agent
#   downloads and verifies them into this container.
# - Run it with --init (reaping init, own PID namespace) and host networking
#   (--network host). Never use --pid=host.
# - Build context must contain the agent binary, named by AGENT_BIN, for
#   example: docker build --build-arg AGENT_BIN=potluck-agent-linux-amd64 .
#   (download it from Potluck, GET /api/pool/release, and check its sha256).
# - The image tag is the release, for example potluck-agent:0.1.0.
FROM nvidia/cuda:13.3.1-runtime-ubuntu24.04

RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*

ARG AGENT_BIN=potluck-agent-linux-amd64
COPY ${AGENT_BIN} /usr/local/bin/potluck-agent
RUN chmod 755 /usr/local/bin/potluck-agent

ENTRYPOINT ["/usr/local/bin/potluck-agent"]

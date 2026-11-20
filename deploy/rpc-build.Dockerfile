# llama-server-rpc image for this fleet.
#
# Builds the llama.cpp `llama-server` binary from the PS5 datacenter fork
# (https://github.com/Jtonna/llama.cpp-for-datacenter-gpu-orchestrator) and
# installs it as `llama-server-rpc`.
#
# Pinned fork commit: a01665d30de2885879a64eae999067ef3ef5fd4b (branch ps5-loader-fixes):
# parallel RPC tensor upload on upstream 0a7675746 (RPC protocol 10), plus two loader
# thread-safety fixes (shared file position on the non-mmap path; async upload map lookup
# on the mmap path). Validated on the cluster on 2026-11-20: output identical to the build
# before the fixes, same load time. The binary deployed that day (sha256 f3867d70...) was
# built from a local archive of the same source tree (5462f64be096).
#
# -DLLAMA_USE_PREBUILT_UI=OFF: the scheduler does not use llama.cpp's web UI, and the
# download of the prebuilt UI fails the build when the commit does not match "latest".
#
# The stock pre-fork variant is kept as `rpc-build.Dockerfile.stock` for rollback.
FROM nvidia/cuda:13.3.1-devel-ubuntu24.04 AS build
ARG LLAMA_REMOTE=https://github.com/Jtonna/llama.cpp-for-datacenter-gpu-orchestrator.git
ARG LLAMA_COMMIT=a01665d30de2885879a64eae999067ef3ef5fd4b
ARG JOBS=12
RUN apt-get update && apt-get install -y --no-install-recommends git cmake ninja-build g++ ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init . && git remote add origin ${LLAMA_REMOTE} \
    && git fetch --depth 1 origin ${LLAMA_COMMIT} && git checkout FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "${LLAMA_COMMIT}"
RUN cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=Release \
      -DLLAMA_USE_PREBUILT_UI=OFF -DGGML_RPC=ON -DGGML_CUDA=ON -DLLAMA_CURL=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=ON \
      -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DCMAKE_CUDA_ARCHITECTURES=120 \
      -DCMAKE_EXE_LINKER_FLAGS=-Wl,-rpath-link=/usr/local/cuda/lib64/stubs \
      -DCMAKE_LIBRARY_PATH=/usr/local/cuda/lib64/stubs \
    && cmake --build build --target llama-server -j ${JOBS}

FROM llama-swap-whisper:local
COPY --from=build /src/build/bin/llama-server /usr/local/bin/llama-server-rpc
# Resolve .local names through the host avahi socket (mounted at /run/avahi-daemon).
RUN apt-get update \
    && apt-get install -y --no-install-recommends libnss-mdns \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i "s/^hosts:.*/hosts: files mdns4_minimal [NOTFOUND=return] dns/" /etc/nsswitch.conf \
    && grep -q mdns4_minimal /etc/nsswitch.conf

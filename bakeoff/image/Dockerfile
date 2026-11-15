# Bake-off sandbox image: python + node + git + zstd + s5cmd + the in-sandbox test scripts.
FROM python:3.12-slim-bookworm
ARG TARGETARCH
ARG S5CMD_VERSION=2.3.0
RUN apt-get update && apt-get install -y --no-install-recommends \
      git zstd curl ca-certificates nodejs npm sqlite3 procps netcat-openbsd iproute2 util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
         arm64) S5=Linux-arm64 ;; amd64) S5=Linux-64bit ;; *) echo "unsupported arch"; exit 1 ;; esac \
    && curl -fsSL "https://github.com/peak/s5cmd/releases/download/v${S5CMD_VERSION}/s5cmd_${S5CMD_VERSION}_${S5}.tar.gz" \
       | tar -xz -C /usr/local/bin s5cmd \
    && pip install --no-cache-dir pytest==8.3.3 \
    && useradd -u 1000 -m -s /bin/bash sandbox \
    && mkdir -p /workspace && chown 1000:1000 /workspace
COPY bin/ /opt/srvm/
RUN chmod +x /opt/srvm/*
ENV PATH=/opt/srvm:$PATH HOME=/home/sandbox
USER 1000
WORKDIR /workspace

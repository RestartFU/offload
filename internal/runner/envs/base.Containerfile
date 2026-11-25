# offload/base: the default job environment (Ubuntu LTS, like GitHub Actions' ubuntu-latest).
# Toolchains (rustup + cargo registry, Go modules and newer Go releases, npm/pip caches) live in
# shared volumes under /opt/offload, not in this image: every environment built on top shares
# them, and refreshing this image never invalidates build output.
FROM docker.io/library/ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update && apt-get install -y --no-install-recommends \
      git openssh-client ca-certificates curl wget jq file patch xz-utils zstd unzip procps less \
      build-essential clang lld cmake ninja-build pkg-config libssl-dev \
      python3 python3-pip python3-venv sudo \
    && rm -rf /var/lib/apt/lists/*
# Go and Node from upstream (Ubuntu's are old). GOTOOLCHAIN=auto fetches newer Go versions a
# project asks for into the shared module cache.
RUN set -e; v=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1); \
    curl -fsSL "https://go.dev/dl/$v.linux-$(dpkg --print-architecture).tar.gz" | tar -xz -C /usr/local; \
    a=$(dpkg --print-architecture | sed 's/amd64/x64/'); \
    f=$(curl -fsSL https://nodejs.org/dist/latest-v24.x/SHASUMS256.txt | grep -o "node-v[0-9.]*-linux-$a.tar.xz" | head -1); \
    curl -fsSL "https://nodejs.org/dist/latest-v24.x/$f" | tar -xJ -C /usr/local --strip-components=1
ENV RUSTUP_HOME=/opt/offload/rustup CARGO_HOME=/opt/offload/cargo \
    GOPATH=/opt/offload/go GOMODCACHE=/opt/offload/go/pkg/mod GOCACHE=/opt/offload/go/cache GOTOOLCHAIN=auto \
    npm_config_cache=/opt/offload/npm PIP_CACHE_DIR=/opt/offload/pip \
    PATH=/opt/offload/cargo/bin:/opt/offload/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# Run by the runner (not inside jobs) after the image is ready, with the build's memory limit:
# installs or repairs the shared toolchains. Idempotent and quick once done.
RUN mkdir -p /etc/offload && printf '%s\n' '#!/bin/sh' 'set -e' \
      'if [ ! -x "$CARGO_HOME/bin/rustup" ]; then' \
      '  echo "offload: installing the Rust toolchain into the shared volume (once per runner)"' \
      '  curl -fsSL https://sh.rustup.rs | sh -s -- -y -q --no-modify-path --profile minimal --default-toolchain none' \
      'fi' \
      'if ! rustc --version >/dev/null 2>&1 || ! cargo clippy --version >/dev/null 2>&1; then' \
      '  rustup toolchain uninstall stable >/dev/null 2>&1 || true' \
      '  rustup -q toolchain install stable --profile minimal -c clippy -c rustfmt' \
      '  rustup -q default stable' \
      'fi' > /etc/offload/bootstrap.sh && chmod +x /etc/offload/bootstrap.sh
LABEL dev.offload.base="1"

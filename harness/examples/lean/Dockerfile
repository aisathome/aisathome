# SPDX-License-Identifier: Apache-2.0
# The sandbox image for the Lean example (and a start for your own): opencode v2 as the entrypoint, python3 and
# CA certificates (the egress forwarder runs in this image too), git, on Debian: Lean's toolchain is linked
# against glibc. Lean itself is not installed here: the harness mounts elan and the toolchain from the host.
#
#   docker build -t harness-lean-example harness/examples/lean
FROM debian:bookworm-slim
ARG OPENCODE_VERSION=2.4.1
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git python3 \
 && rm -rf /var/lib/apt/lists/*
# The harness drives `opencode run --standalone --auto --format json`: opencode v2 (the v1.x releases lack
# --standalone). The release's own SHA256SUMS is checked before the binary is unpacked.
RUN set -eu; cd /tmp; \
    base="https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}"; \
    curl -fsSLO "$base/opencode-linux-x64.tar.gz"; curl -fsSLO "$base/SHA256SUMS"; \
    grep ' opencode-linux-x64.tar.gz$' SHA256SUMS | sha256sum -c -; \
    tar -xzf opencode-linux-x64.tar.gz -C /usr/local/bin opencode; \
    rm -f opencode-linux-x64.tar.gz SHA256SUMS; opencode --version
ENTRYPOINT ["opencode"]

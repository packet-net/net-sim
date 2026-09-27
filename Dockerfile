# Multi-stage build for the net-sim simulator (sim-router + sim-web).
#
# Builder stage: clones samoyed, compiles it; builds the Go binaries
# from this checkout.
#
# Runtime stage: a slim image carrying just the binaries, the
# samoyed-direwolf runtime libraries (libportaudio2 et al.), direwolf and
# pdn-soundmodem for the other two TNC backends, and the
# default two-node network at /etc/sim/network.yaml. Entrypoint is
# sim-web on :8080.
#
# Build:
#   docker build -t net-sim:dev .
#
# Run with the bundled two-node network:
#   docker run --rm -p 8080:8080 -p 8001:8001 -p 8002:8002 net-sim:dev
#
# Run with a custom network (any KISS port range):
#   docker run --rm --network=host \
#     -v ./my-network.yaml:/etc/sim/network.yaml \
#     net-sim:dev
#
# Published image: ghcr.io/packet-net/net-sim
# Tags: :main, :main-<sha>, :v<x.y.z>, :latest (on tagged release)

# ---- builder ------------------------------------------------------------
# samoyed's go.mod requires Go ≥ 1.25 (as of mid-2025). Bump in lockstep
# with samoyed's minimum.
FROM golang:1.25-bookworm AS builder

# samoyed source, pinned via build args. Both default here so every build
# path (local `docker build`, install.sh, CI) gets the same samoyed.
#
# TEMPORARY PIN: KISS ACKMODE is not yet in doismellburning/samoyed; it lives
# as a single additive commit on the fork's main (M0LTE/samoyed @ 61bb965,
# upstream main + ACKMODE). When ACKMODE lands in doismellburning/samoyed,
# revert SAMOYED_REPO to https://github.com/doismellburning/samoyed.git and set
# SAMOYED_REF back to `main` (or that commit). Pinned to a fixed SHA so release
# images are reproducible rather than tracking a floating branch.
ARG SAMOYED_REPO=https://github.com/M0LTE/samoyed.git
ARG SAMOYED_REF=61bb965747c5fce1b7aeb05acb5a43b23911375b

RUN apt-get update && apt-get install -y --no-install-recommends \
        git make pkg-config gcc libc6-dev \
        libudev-dev libhamlib-dev portaudio19-dev \
        libavahi-client-dev libbsd-dev libgps-dev libasound2-dev \
    && rm -rf /var/lib/apt/lists/*

# Accepts either a branch name OR a commit SHA in SAMOYED_REF: we always
# init+fetch a single commit, so SHAs work without `git clone --branch`
# (which rejects them). A pinned SHA keeps this layer's cache key stable, so
# rebuilding a given net-sim tag reproduces the same samoyed snapshot.
RUN git init --quiet /src/samoyed \
    && git -C /src/samoyed remote add origin "${SAMOYED_REPO}" \
    && git -C /src/samoyed fetch --depth 1 origin "${SAMOYED_REF}" \
    && git -C /src/samoyed checkout --quiet FETCH_HEAD \
    && make -C /src/samoyed cmds

# net-sim — copy source and build.
WORKDIR /src/net-sim
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /out/sim-router ./cmd/sim-router \
 && go build -ldflags="-s -w" -o /out/sim-web    ./cmd/sim-web

# ---- runtime ------------------------------------------------------------
FROM debian:bookworm-slim AS runtime

# pdn-soundmodem (the tnc: pdn backend) comes from the packet-net apt
# repository. Empty PDN_VERSION takes the newest; set it (e.g. 0.80.0) to pin.
ARG PDN_VERSION=

RUN apt-get update && apt-get install -y --no-install-recommends \
        libportaudio2 libhamlib4 libudev1 libavahi-client3 \
        libbsd0 libgps28 libasound2 libjack-jackd2-0 libpulse0 \
        direwolf \
        ca-certificates curl gpg \
    && curl -fsSL https://packet-net.github.io/apt/pubkey.asc \
        | gpg --dearmor -o /usr/share/keyrings/packet-net.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/packet-net.gpg] https://packet-net.github.io/apt ./" \
        > /etc/apt/sources.list.d/packet-net.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends "pdn-soundmodem${PDN_VERSION:+=$PDN_VERSION}" \
    && apt-get purge -y --auto-remove curl gpg \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/* /var/log/*

# binaries
COPY --from=builder /out/sim-router         /usr/local/bin/sim-router
COPY --from=builder /out/sim-web            /usr/local/bin/sim-web
COPY --from=builder /src/samoyed/dist/samoyed-direwolf /usr/local/bin/samoyed-direwolf

# default network — overridable via volume mount
COPY configs/two-node.yaml /etc/sim/network.yaml

# Web UI; default config writes KISS ports 8001 and 8002. Custom configs
# may bind anywhere — use --network=host (or `-p ...`) accordingly.
EXPOSE 8080 8001 8002

# A non-root user for the running process.
RUN useradd --system --no-create-home --shell /usr/sbin/nologin sim

# NOTE on -rt-priority: it renices the router + TNC children, which needs
# CAP_SYS_NICE. Grant it at RUNTIME with `--cap-add SYS_NICE` (and run the
# container as root, or with that cap in the user-namespace) — NOT with file
# capabilities on the binaries. A `setcap cap_sys_nice+ep` here makes the
# binary REFUSE TO exec (EPERM "operation not permitted") on any host whose
# bounding set lacks the cap — e.g. an unprivileged LXC — breaking the image
# for everyone who isn't using -rt-priority. -rt-priority already degrades
# gracefully (logs a one-line warning and runs at normal priority) where the
# platform won't grant the cap, so the binary must stay capability-free.
USER sim

ENTRYPOINT ["/usr/local/bin/sim-web", "-addr", ":8080", "-config", "/etc/sim/network.yaml"]
# Default to autostart so a one-line `docker run` produces a running
# topology on the bundled config. Override by passing your own args
# (e.g. `... net-sim:main -autostart=false`) if you want to drive
# Start/Stop from the web UI or API.
CMD ["-autostart"]

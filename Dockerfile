# Multi-stage build for ace + voip_patrol in one image.
#
# Stage 1 builds voip_patrol from source (matches the upstream
# voip_patrol/docker/Dockerfile flow). We copy from the local
# checkout rather than cloning so any local edits (incl. the
# vp_call_param null-pointer fix at f92b7eb) ride along.
#
# Stage 2 builds the ace Go binary.
#
# Stage 3 is the runtime image: Debian + the slim set of libs voip_patrol
# links against + both binaries + ace's templates/static.
#
# Network model: meant to run with `network_mode: host` in compose, so
# we don't EXPOSE any ports — voip_patrol binds whatever -port it's
# told, ace listens on whatever -addr it's told, both visible on the
# host's interfaces.

# --- Stage 1: voip_patrol -----------------------------------------------------
FROM debian:trixie AS voip_patrol_builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential libcurl4-openssl-dev cmake pkg-config \
        libasound2-dev libopus0 libopus-dev libssl-dev libuuid1 uuid-dev \
        git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Copy local checkout (includes our null-pointer fix). The build is
# self-contained: pjproject lives under voip_patrol/pjproject/ as a
# submodule, already pulled. The `voip_patrol` build context is wired
# in docker-compose.yml's additional_contexts.
COPY --from=voip_patrol . /git/voip_patrol

RUN cd /git/voip_patrol \
    && rm -rf CMakeCache.txt CMakeFiles cmake_install.cmake Makefile \
    && cp include/config_site.h pjproject/pjlib/include/pj/config_site.h \
    && cd pjproject && ./configure --disable-libwebrtc --disable-opencore-amr \
    && make dep && make && make install \
    && cd .. && cmake CMakeLists.txt && make

# --- Stage 2: ace -------------------------------------------------------------
FROM golang:1.22-bookworm AS ace_builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/ace .

# --- Stage 3: runtime ---------------------------------------------------------
FROM debian:trixie-slim

# Runtime libs only — no toolchain. The list below matches what
# voip_patrol's pjproject was linked against (libcurl/libssl/libopus/
# libasound for ALSA stubs even when running headless).
RUN apt-get update && apt-get install -y --no-install-recommends \
        libcurl4 libssl3 libopus0 libasound2t64 libuuid1 ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# voip_patrol binary + reference WAVs (needed by some scenarios for the
# `play` action). Mirror the upstream image's symlink so XML paths like
# `voice_ref_files/reference_8000.wav` resolve.
COPY --from=voip_patrol_builder /git/voip_patrol/voip_patrol /usr/local/bin/voip_patrol
COPY --from=voip_patrol_builder /git/voip_patrol/voice_ref_files /voice_ref_files

# ace binary + templates + static. We don't bake scenarios or runs into
# the image; both are bind-mounted from the host in compose.
COPY --from=ace_builder /out/ace /usr/local/bin/ace
COPY templates /app/templates
COPY static /app/static

# Scratch dir where voip_patrol writes recordings during a run. Each
# run's spawn sets cwd to its own runs/<id>/ dir, so this is mostly a
# fallback for ad-hoc invocations.
RUN mkdir -p /voice_files

WORKDIR /app
ENV ACE_ADDR=0.0.0.0:8086 \
    ACE_VOIP_PATROL_BIN=/usr/local/bin/voip_patrol \
    ACE_SCENARIOS_DIR=/data/scenarios \
    ACE_RUNS_DIR=/data/runs

# Translate the env-var contract into flags. Public address is
# optional; we only pass --public-address when ACE_PUBLIC_ADDRESS is
# non-empty so the binary doesn't get a stray empty flag.
CMD ["/bin/sh", "-c", "exec /usr/local/bin/ace \
    -addr ${ACE_ADDR} \
    -voip-patrol-bin ${ACE_VOIP_PATROL_BIN} \
    -voip-patrol-port ${ACE_VOIP_PATROL_PORT:-5093} \
    -scenarios-dir ${ACE_SCENARIOS_DIR} \
    -runs-dir ${ACE_RUNS_DIR} \
    ${ACE_PUBLIC_ADDRESS:+-public-address ${ACE_PUBLIC_ADDRESS}}"]

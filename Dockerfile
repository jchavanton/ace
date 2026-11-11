# Multi-stage build for ace + voip_patrol in one image.
#
# Stage 1 is the upstream voip_patrol release image — we just pull the
# compiled binary + reference WAVs out of it rather than building from
# source. Bump the pin when a new voip_patrol tag is out.
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

# --- Stage 1: voip_patrol (prebuilt) ------------------------------------------
# One source of truth for the version string: ARG here, stamped into
# ACE_VOIP_PATROL_VERSION in stage 3 so ace can render it in the UI.
ARG VOIP_PATROL_VERSION=1.0.1
FROM jchavanton/voip_patrol:${VOIP_PATROL_VERSION} AS voip_patrol_image

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
        iptables \
    && rm -rf /var/lib/apt/lists/*

# voip_patrol binary + reference WAVs (needed by some scenarios for the
# `play` action). Mirror the upstream image's symlink so XML paths like
# `voice_ref_files/reference_8000.wav` resolve.
COPY --from=voip_patrol_image /vp/voip_patrol /usr/local/bin/voip_patrol
COPY --from=voip_patrol_image /vp/voice_ref_files /voice_ref_files

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
# Re-declare ARG in this stage so the FROM-time value is in scope here;
# ARGs reset at each FROM in a multi-stage build.
ARG VOIP_PATROL_VERSION=1.0.1
ENV ACE_ADDR=0.0.0.0:8086 \
    ACE_VOIP_PATROL_BIN=/usr/local/bin/voip_patrol \
    ACE_VOIP_PATROL_VERSION=${VOIP_PATROL_VERSION} \
    ACE_SCENARIOS_DIR=/data/scenarios \
    ACE_RUNS_DIR=/data/runs \
    ACE_BOTS_DIR=/data/bots \
    ACE_FIREWALL_STATE_DIR=/data/firewall

# Translate the env-var contract into flags. Public address is
# optional; we only pass --public-address when ACE_PUBLIC_ADDRESS is
# non-empty so the binary doesn't get a stray empty flag.
CMD ["/bin/sh", "-c", "exec /usr/local/bin/ace \
    -addr ${ACE_ADDR} \
    -voip-patrol-bin ${ACE_VOIP_PATROL_BIN} \
    -voip-patrol-port ${ACE_VOIP_PATROL_PORT:-5093} \
    -rtp-port-start ${ACE_RTP_PORT_START:-4000} \
    -rtp-port-end ${ACE_RTP_PORT_END:-14000} \
    -scenarios-dir ${ACE_SCENARIOS_DIR} \
    -runs-dir ${ACE_RUNS_DIR} \
    -bots-dir ${ACE_BOTS_DIR} \
    -runs-retention-days ${ACE_RUNS_RETENTION_DAYS:-30} \
    -alerts-retention-count ${ACE_ALERTS_RETENTION_COUNT:-500} \
    ${ACE_PUBLIC_BASE_URL:+-public-base-url ${ACE_PUBLIC_BASE_URL}} \
    -firewall-state-dir ${ACE_FIREWALL_STATE_DIR} \
    ${ACE_PUBLIC_ADDRESS:+-public-address ${ACE_PUBLIC_ADDRESS}} \
    ${ACE_LOCAL_IPS:+-local-ips ${ACE_LOCAL_IPS}} \
    ${ACE_FIREWALL_SAVE_PATH:+-firewall-save-path ${ACE_FIREWALL_SAVE_PATH}} \
    ${ACE_BASIC_AUTH_HTPASSWD:+-basic-auth-htpasswd ${ACE_BASIC_AUTH_HTPASSWD}} \
    ${ACE_TLS_CERT:+-tls-cert ${ACE_TLS_CERT}} \
    ${ACE_TLS_PRIVKEY:+-tls-privkey ${ACE_TLS_PRIVKEY}} \
    ${ACE_TLS_CALIST:+-tls-calist ${ACE_TLS_CALIST}} \
    ${ACE_PROMETHEUS_URL:+-prometheus-url ${ACE_PROMETHEUS_URL}} \
    ${ACE_WEBHOOK_URL:+-webhook-url ${ACE_WEBHOOK_URL}} \
    ${ACE_WEBHOOK_SECRET:+-webhook-secret ${ACE_WEBHOOK_SECRET}} \
    ${ACE_WEBHOOK_TIMEOUT:+-webhook-timeout ${ACE_WEBHOOK_TIMEOUT}}"]

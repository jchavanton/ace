# ace

<p align="center">
  <img src="static/logo.png" alt="ace logo" width="200">
</p>

Orchestration for [voip_patrol](https://github.com/jchavanton/voip_patrol). Schedules scenarios as recurring
"bots", runs them, evaluates verdicts
on the output, fires and recovers alerts over SMTP, exposes Prometheus
metrics. A browser UI and a small JSON API drive the same engine.

Design notes (historical) in [SPEC.md](SPEC.md).

## Features

- **Scenarios** — list, edit, create, delete XML scenarios under `-scenarios-dir`.
- **Runs** — start a scenario, follow stdout and pjsua debug log live,
  stop in-flight, delete finished. Multiple runs can execute
  concurrently as long as their SIP port and RTP range don't collide.
- **Recordings** — WAVs produced by voip_patrol's `record="true"` are
  listed on the run page and served inline via `<audio>`.
- **Verdict** — scenarios can ship a `<name>.verdict.json` sidecar
  (e.g. audio-energy thresholds); the verdict decides pass/fail after
  the run finishes.
- **Bots** — enable a scenario as a scheduled bot (cron-ish interval);
  failures raise an alert, recoveries clear it.
- **Alerts** — in-UI history with ack, SMTP email on
  firing/reminder/recovery (configurable retention).
- **Prometheus** — `/metrics` exposes run/call counters, INVITE→200
  latency, MOS, RTT, loss. `/bots/:name/report` embeds Grafana-style
  range charts when `-prometheus-url` is set.
- **Firewall** — optional `/firewall` page to edit iptables rules
  through the UI, apply them to the running kernel, and persist via
  `iptables-save`.
- **Retention** — hourly sweep removes finished runs older than
  `-runs-retention-days` and trims `alerts.json` to
  `-alerts-retention-count` rows.
- **Auth / TLS** — optional basic auth (htpasswd/bcrypt) and TLS
  cert/key passthrough to voip_patrol.

## Quick start

```
go build -o ace .

./ace -addr 0.0.0.0:8086 \
      -voip-patrol-bin /usr/local/bin/voip_patrol \
      -public-address <public ip for Contact/Via> \
      -scenarios-dir ./scenarios \
      -runs-dir ./runs
```

Drop XML scenarios under `./scenarios/` (any `.xml` file is picked up).

Open `http://localhost:8086/`.

A `Dockerfile` and `docker-compose.yml` are included; an Ansible role
lives under `ansible/`.

## Flags

```
-addr                       HTTP bind (default 0.0.0.0:8086)
-voip-patrol-bin            path to voip_patrol (default /usr/local/bin/voip_patrol)
-voip-patrol-port           default local SIP port (default 5093, per-run override)
-rtp-port-start             default RTP range start (default 4000)
-rtp-port-end               default RTP range end (default 14000)
-default-timeout-seconds    wall-clock cap per run when scenario sets none; 0 = unlimited
-public-address             public IP for SIP Contact/Via (NAT)
-local-ips                  comma-separated private IPs to show in the Network dropdown
                            (overrides interface enumeration; needed on cloud VMs)
-scenarios-dir              XML scenarios dir (default ./scenarios)
-runs-dir                   per-run output dir (default ./runs)
-bots-dir                   bots + alerts.json dir (default ./bots)
-voice-ref-dir              reference WAV source dir, symlinked into each run
                            (default /voice_ref_files; empty disables)
-runs-retention-days        age in days before finished runs are swept (default 30; 0 disables)
-alerts-retention-count     cap on alerts.json rows (default 500; 0 disables)
-public-base-url            externally-visible prefix used in alert emails
                            (e.g. https://ace.example.com; empty = no link)
-basic-auth-htpasswd        htpasswd file (bcrypt); empty = no auth
-firewall-state-dir         dir holding firewall.json for /firewall; empty disables the page
-firewall-save-path         iptables-save target for reboot persistence
                            (e.g. /etc/iptables/rules.v4; empty = runtime only)
-tls-cert                   TLS cert (pem) for voip_patrol; needs -tls-privkey
-tls-privkey                TLS private key (pem)
-tls-calist                 TLS CA list (pem); optional
-prometheus-url             Prometheus base URL for bot report charts
                            (e.g. http://localhost:9090; empty disables charts)
```

## API

For scripts such as a post-deploy check. Shares basic auth with the UI.

| Request | Response |
|---|---|
| `POST /api/scenarios/<name>/run` | `202` `{"passed": null, "run": {...}}`, `Location: /api/runs/<id>`. Uses the scenario's saved ports; `409` if the ports are busy, `404` if the scenario doesn't exist. |
| `GET /api/runs/<id>` | `200` `{"passed": true\|false\|null, "run": {...}}`. `passed` is `null` while running, then `true` only if the run finished (`done`) with at least one call, every call PASSed, and the scenario's verdict (`<name>.verdict.json`) agrees. `run.calls[].reason` says why a call failed. |
| `GET /api/metrics/range?...` | Prometheus range query proxy used by `/bots/:name/report`; only available when `-prometheus-url` is set. |

Prometheus scrape endpoint is `/metrics` (standard client_golang
format).

Run a scenario and exit non-zero unless it passes:

```sh
#!/bin/sh
# usage: ace-check.sh <ace-url> <scenario>   e.g. ace-check.sh http://ace:8086 probe
# ACE_AUTH=user:pass when basic auth is on; ACE_TIMEOUT seconds to wait (default 600).
set -eu
api() { if [ -n "${ACE_AUTH:-}" ]; then curl -u "$ACE_AUTH" "$@"; else curl "$@"; fi; }
started=$(api -sS -X POST "$1/api/scenarios/$2/run")
id=$(printf '%s\n' "$started" | jq -r '.run.id // empty' 2>/dev/null || true)
[ -n "$id" ] || { echo "start failed: $started" >&2; exit 2; }
deadline=$(( $(date +%s) + ${ACE_TIMEOUT:-600} ))
while :; do
  body=$(api -fsS "$1/api/runs/$id")
  passed=$(printf '%s\n' "$body" | jq -r .passed)
  [ "$passed" != null ] && break
  [ "$(date +%s)" -lt "$deadline" ] || { echo "run $id still running" >&2; exit 3; }
  sleep 2
done
printf '%s\n' "$body" | jq -r '.run.calls[]? | "\(.label): \(.result) \(.reason // "")"'
[ "$passed" = true ]
```

## Notes

- Concurrency is gated by port allocation: a new run is accepted only
  if its SIP port and RTP range don't overlap with any in-flight run.
  The nav shows how many runs are currently in flight.
- voip_patrol's `record="true"` attribute on the call action produces
  `record_*.wav` files in each run's directory; the UI lists them and
  serves them via `<audio>` playback.
- `results.json` is append-only inside voip_patrol; ace sets cwd per-run
  so each run gets its own clean file under `runs/<id>/`.
- When `-voice-ref-dir` is non-empty, each run dir gets a
  `voice_ref_files` symlink pointing there so scenarios can reference
  playback WAVs with a stable relative path.

## License

Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

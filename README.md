# ace

<p align="center">
  <img src="static/logo.png" alt="ace logo" width="200">
</p>

Web admin for driving `voip_patrol` test scenarios:
list scenarios, run them, capture per-call pass/fail + RTP/SIP stats,
play back recorded WAVs.

See [SPEC.md](SPEC.md).

## Quick start

```
go build -o ace .

./ace -addr 0.0.0.0:8086 \
      -voip-patrol-bin /git/voip_patrol/voip_patrol \
      -public-address 24.122.254.8 \
      -scenarios-dir ./scenarios \
      -runs-dir ./runs
```

Drop XML scenarios under `./scenarios/` (any `.xml` file is picked up).

Open `http://localhost:8086/`.

## Flags

```
-addr                  HTTP bind (default 0.0.0.0:8086)
-voip-patrol-bin       path to voip_patrol (default /usr/local/bin/voip_patrol)
-voip-patrol-port      local SIP port (default 5093)
-public-address        public IP for Contact/Via (NAT)
-scenarios-dir         XML scenarios dir (default ./scenarios)
-runs-dir              per-run output dir (default ./runs)
```

## API

For scripts such as a post-deploy check. Same basic auth as the UI.

| Request | Response |
|---|---|
| `POST /api/scenarios/<name>/run` | `202` `{"passed": null, "run": {...}}`, `Location: /api/runs/<id>`. Uses the scenario's saved ports. `409` if the ports are busy, `404` if the scenario doesn't exist. |
| `GET /api/runs/<id>` | `200` `{"passed": true\|false\|null, "run": {...}}`. `passed` is `null` while running, then `true` only if the run finished (`done`) with at least one call and every call PASSed, after the scenario's verdict (`<name>.verdict.json`). `run.calls[].reason` says why a call failed. |

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

- One run at a time. The nav shows a "run in progress" badge.
- voip_patrol's `record="true"` attribute on the call action produces
  `record_*.wav` files in each run's directory; the UI lists them and
  serves them via `<audio>` playback.
- `results.json` is append-only inside voip_patrol; we set cwd per-run
  so each run gets its own clean file under `runs/<id>/`.

## License

Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

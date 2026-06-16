# ace — voip_patrol controller (v1 spec)

WWI fighter-ace small admin UI that runs `voip_patrol` scenarios and shows
results.

## Purpose

Drive voip_patrol from a web UI instead of remembering the right
`docker exec ...` invocation; persist run history; show per-call
pass/fail with SIP/RTP stats and playable WAV recordings.

Inspired by the layout of `~/git/netic-ai/sip-proxy/web-admin`.

## Scope (v1)

In scope:
- List XML scenarios from a configured directory.
- Run one selected scenario via `exec.Command("voip_patrol", ...)`.
- Stream voip_patrol output to a per-run log file.
- Parse `results.json` after the run, store as `run.json`.
- Show per-call pass/fail, cause code, latency, MOS, RTT.
- Aggregate (p50/p95 invite-200, MOS avg, packet loss).
- Play recorded WAVs inline (`<audio>` element).
- Serialize runs (one at a time); show busy state in the nav.

Not in scope (v2+):
- Scenario authoring UI / form-based editing. Today: drop XML in `scenarios/`.
- Audio content validation via ASR (POST WAVs to a vxml_asr sidecar).
- Energy / silence / DTMF echo validators.
- Authentication. Bind to private LAN.
- Multi-host orchestration. Single voip_patrol on the same host.
- Concurrent runs. v1 serializes via mutex.

## Architecture

```
  ┌──────────────────────────────────────────────────┐
  │                    ace (gin)                     │
  │                                                  │
  │   handlers ───▶ models ─── scenario.xml          │
  │                                                  │
  │   handlers ───▶ controller ──▶ voip_patrol ─SIP─▶│
  │                     │                            │
  │                     ▼                            │
  │              runs/<id>/                          │
  │                ├ run.json    (parsed)            │
  │                ├ results.json (raw vp output)    │
  │                ├ stdout.log                      │
  │                └ record_*.wav                    │
  └──────────────────────────────────────────────────┘
```

## Layout

```
config/             flags + resolved config
controller/         exec voip_patrol, parse results, compute aggregates
models/             Scenario, Run, CallResult, Aggregate
handlers/           gin routes
templates/          layout + 4 content templates
static/             css
scenarios/          XML files (operator-managed)
runs/<timestamp-scenario>/   per-run output
main.go             entry point
```

## Run lifecycle

1. POST `/scenarios/:name/run`
2. Mutex acquired; busy state surfaces in nav
3. `runs/<timestamp>-<name>/` dir created
4. `voip_patrol --port <port> -c <abs path> [--public-address <ip>]`
   spawned with cwd = the run dir
5. stdout/stderr → `stdout.log`
6. On exit: read `results.json`, parse line-by-line, filter to this
   scenario's label, compute aggregates, write `run.json`
7. Redirect to `/runs/:id`

## Storage

Filesystem only. SQLite if listing becomes slow (probably never with the
volumes we'd see).

`results.json` is append-only across runs in voip_patrol itself —
mitigated by setting the spawn's cwd to the per-run dir so each run gets
its own file.

## Estimate

v1: scaffolded in a session. Iteration after that depends on what
validators we want.

# ace

WWI fighter-ace style. Web admin for driving `voip_patrol` test scenarios:
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

## Notes

- One run at a time. The nav shows a "run in progress" badge.
- voip_patrol's `record="true"` attribute on the call action produces
  `record_*.wav` files in each run's directory; the UI lists them and
  serves them via `<audio>` playback.
- `results.json` is append-only inside voip_patrol; we set cwd per-run
  so each run gets its own clean file under `runs/<id>/`.

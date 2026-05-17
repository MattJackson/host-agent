# Contributing to host-agent

Thanks for considering a contribution! This project is small and
focused — keeping it that way is a feature, so please read this before
opening anything substantial.

## What's in scope

- Bug fixes — particularly around new hardware (chassis profiles, GPU
  models, drive types) that the agent silently mis-handles.
- New chassis profiles (`profiles/<model>.env`). These need IPMI sensor
  names + the chassis' fan-floor behavior. See "Adding a chassis profile"
  below.
- New sensor backends in `internal/sensors/`. If you have a non-NVIDIA
  GPU or a non-IPMI fan-control path (e.g. Supermicro's `raw 0x30 0x70
  0x66 …`), a clean implementation behind the existing interfaces is
  welcome.
- Improvements to the control math in `internal/control/` — must come
  with unit tests and a clear before/after.
- Docs improvements.

## What's *out* of scope

- **External Go dependencies.** The binary is intentionally
  zero-dependency (no `go.sum` shipped, vendor-free build). Anything
  that adds an `import "github.com/..."` needs a strong justification.
- **Lookup tables / piecewise fan curves.** The controller is dynamic
  and derived-from-constants by design — temp ranges → continuous math
  → fan speed. No hardcoded `"if temp > 80 then fan = 60"` rules.
- **Replacing the IPMI/smartctl/nvidia-smi subprocess approach with a
  library binding.** The subprocess pattern is what makes the image
  trivially portable. Native bindings would pull in C deps, kill
  CGO-disabled builds, and explode the image size.
- **Features that only make sense for a specific cloud or paid
  service.** This project is hardware-and-Prometheus-shaped; please
  keep it that way.

## Dev environment

You need Go 1.23+ and Docker. No other tooling.

```sh
git clone https://github.com/mattjackson/host-agent
cd host-agent
go test ./...                          # unit + e2e (no Docker needed)
docker build -t host-agent:dev .       # full image build
```

Most logic lives in `internal/control/` (pure functions) and is exhaustively
unit-testable without any I/O. Subprocess sensors are mocked via the
`runner` interface — see `internal/runner/fake.go` and the testdata
under `internal/sensors/testdata/`.

## Code style

- `gofmt -s` (run by CI; PRs failing it are blocked).
- No external Go deps. Standard library only.
- Comments should explain *why* (a non-obvious constraint, a workaround
  for vendor behavior, an invariant). Don't restate what the code does.
- Keep functions small and pure where possible. The "main loop" lives in
  `internal/controller/`; everything it calls is testable in isolation.

## Adding a chassis profile

1. Determine the `dmidecode` product slug for your chassis:

   ```sh
   sudo dmidecode -s system-product-name
   # → "PowerEdge R730xd" → slug "r730xd"
   ```

   The slug is what `detect_model` produces:
   `lowercase(strip("PowerEdge "), then replace non-[a-z0-9] with _,
   then strip trailing _)`.

2. Add `profiles/<slug>.env`. Minimum content is `MIN_FAN`:

   ```sh
   : "${MIN_FAN:=15}"   # the lowest PWM the BMC actually respects.
                        # Below this it either clamps to a safety RPM
                        # (Dell R7xx series) or obeys literally and
                        # stalls fans (Dell R4xx series). Test before
                        # picking a value.
   ```

3. (Optional) Add IPMI sensor mappings so the dashboard's
   per-CPU/inlet/exhaust panels resolve. See `profiles/r730xd.env`:

   ```sh
   : "${SENSOR_CPU1_NAME:=Temp}"
   : "${SENSOR_CPU1_ID:=26}"
   : "${SENSOR_CPU2_NAME:=Temp}"
   : "${SENSOR_CPU2_ID:=27}"
   : "${SENSOR_INLET_NAME:=Inlet Temp}"
   : "${SENSOR_EXHAUST_NAME:=Exhaust Temp}"
   ```

   The IDs come from `ipmi-sensors --output-sensor-state` on the actual
   hardware. Run it once and grep for the temp sensors.

4. Test on real hardware: deploy a `host-agent:dev` build, watch
   `docker logs host-agent` for the `[fan-controller] chassis=<slug>`
   line and confirm the right profile loaded. Then watch one full
   24-hour EWMA settling cycle before opening the PR.

5. PR description should include:
   - chassis model + iDRAC/BMC firmware version
   - `dmidecode -s system-product-name` output
   - the fan-floor behavior you observed (which PWM values do what)
   - a screenshot or paste of `/var/lib/host-agent/state/metrics.prom`
     after the controller has been running for at least a few hours.

## Submitting PRs

- One change per PR. Don't bundle a profile addition with a controller
  refactor.
- Reference any related issue in the description.
- Make sure `go test ./...` passes.
- CI runs `gofmt -s -l` and `go vet ./...`; PRs failing either are
  blocked.
- Commit messages: imperative mood, ~70 char subject, blank line, body
  if needed. No "Co-Authored-By: AI" trailers — keep authorship clean.

## Reporting bugs

See [SECURITY.md](SECURITY.md) for vulnerability reports (please don't
file public issues for those).

For functional bugs, open a GitHub issue with:

- which install method (compose, `docker run`, Unraid, install.sh)
- chassis (`dmidecode -s system-product-name`) + which profile loaded
  (visible in container logs as `[fan-controller] chassis=…`)
- `docker logs host-agent --tail 200` output around the bad behavior
- contents of `/var/lib/host-agent/state/metrics.prom`
- expected vs actual behavior

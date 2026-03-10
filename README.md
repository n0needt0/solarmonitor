# SolarControl

Go service managing 4x Schneider XW Pro 6848 inverters with SunVault LFP batteries. Charges from solar during the day (ramping up to 16kW based on available export), discharges at a fixed rate overnight, and prevents either grid leg from exporting at night.

## System Overview

```
                    USB RS-485 (/dev/ttyUSB0)
New WattNode WND-WR-MB ──────────────── Raspberry Pi (ARM64)
(dedicated grid meter)                      │
  │                                         │  solarcontrol (systemd)
  │ CTs on mains (L1 + L2)                │
  │                                         ├── Modbus TCP :502/:503 → Insight Gateway
  │                                         └── HTTPS → Telegram Bot API
  │
  │                                 Insight Facility Gateway
  │                                    192.168.86.86
  │                                         │ XanBus
  │                                         ▼
  │                              ┌────────────────────┐
  │                              │   4x XW Pro 6848   │
  │                              │   Unit IDs 10-13   │
  │                              ├────────────────────┤
  │                              │   4x SunVault      │
  │                              │   78 kWh (3x per)  │
  │                              └────────────────────┘
  │
Existing WattNode (S/N 9308266)
  └── RS-485 → Insight (native, read-only fallback)
```

## Hardware

| Component | Spec |
|-----------|------|
| Inverters | 4x XW Pro 6848 — unit IDs 10 (master), 11, 13, 12 (slaves) |
| Batteries | 4x SunVault, 3x 6.5kWh M0020 per vault = 78 kWh total (~58 kWh usable) |
| Charge breaker | 80A = 16kW max |
| Solar arrays | ~19.5kW peak combined |
| Controller | Raspberry Pi (ARM64), systemd service |
| Gateway | Insight Facility at 192.168.86.86 (Modbus TCP + HTTPS API) |

## Two WattNodes

The system uses two WattNode meters for grid power monitoring:

### USB WattNode (Primary) — WND-WR-MB

- Connected to Raspberry Pi via USB RS-485 adapter (`/dev/ttyUSB0`)
- Dedicated CTs on mains L1 + L2
- Modbus RTU, unit ID 1, 19200 baud
- Read every 10 seconds — all charge/discharge decisions use this meter
- Split-phase config: ConnectionType=2 (1P3W 120/240V), PhaseOffset=180
- Scale factor 0.5 applied (reads 2x actual — hardware quirk, root cause TBD)

### Insight Native WattNode (Fallback) — S/N 9308266

- Connected to Insight Facility gateway via RS-485 (XanBus)
- Same meter type, mounted with different CT orientation
- Visible in Insight web UI and accessible via Modbus TCP port 503
- Not actively polled by solarcontrol — available as read-only fallback
- If the USB WattNode fails, the service enters SAFE mode (idles all inverters)

### Why Two Meters

The USB WattNode gives the Pi direct, low-latency grid readings without competing for the Insight Modbus bus. Insight's native WattNode shares the same bus as inverter commands, and reading it would add delay to the 2-second write gating. Keeping reads local (USB) and writes remote (TCP) eliminates bus contention.

## State Machine

```
IDLE ─────────────────────────────┐
  │                               │
  ├─ charge window ──► DAY_CHARGE │
  │                       │       │
  │   ┌── export > 1kW ──┘       │
  │   │   ramp with sun          │
  │   │   dead band: 0-1kW exp   │
  │   │              0-2kW imp   │
  │   │                          │
  │   └── end of charge window ──┤
  │                               │
  ├─ discharge window ──► NIGHT_DISCHARGE
  │                           │
  │   ┌── any leg export ─────┘
  │   │   idle inverters one by one
  │   ▼
  │  NIGHT_REDUCED
  │   │  monitor, can only reduce further
  │   │  resume if import > threshold sustained 5 min
  │   │
  │   └── all 4 idled + solar exporting ──► DAY_CHARGE (early charge)
  │
  ├── /stop ──► STOPPED (manual, no auto-resume)
  │              └── /start ──► evaluate time → DAY_CHARGE or NIGHT_DISCHARGE
  │
  └── WattNode fail x5 ──► SAFE (idle all, monitor for recovery)
```

## Day Charge Logic

- Waits for grid export > 1kW before starting (avoids false starts)
- Starts at 600W/inv (2.4kW total)
- Ramps aggressively: takes all available export every 30 seconds
- Trims gently: halves overshoot above 2kW import, 5-minute hold
- Dead band: 0-1kW export / 0-2kW import = no action (absorbs cloud transients)
- Max: 4000W/inv (16kW total, capped by 80A breaker)
- SOC-based charge balancing: lower SOC banks get higher rates
- Day peak shave: if sustained high import during charge window, temporarily discharges

## Night Discharge Logic

- Fixed 600W/inv base (2.4kW total)
- Zero-tolerance export guard: any leg exporting = immediate action
- Idles inverters one at a time (slaves first: 12 → 13 → 11, master 10 last)
- Rebalances remaining inverters at reduced rate with SOC balancing
- Dynamic resume: if grid import > 2x the step increase sustained for 5 minutes, restores one inverter
- If all 4 idled and solar is exporting, transitions to early DAY_CHARGE (with manual override to prevent oscillation)

## Modbus Communication

### Write Port 502 (XW Pro EPC Commands)

| Register | Description |
|----------|-------------|
| 40210 | EPC Charge Max Power (watts) |
| 40213 | EPC Mode: 0=idle, 1=charge, 2=discharge |
| 40152 | EPC Max Discharge Power (watts) |
| 40149 | Recharge SOC (% x 10) |

### Read Port 503 (BMS + Status)

| Register | Description |
|----------|-------------|
| 960-1001 | BMS bulk read (unit ID 1): SOC + battery power for all 4 inverters |

- 2-second minimum gap between writes (Schneider requirement)
- Keepalive every 60 seconds (resend last command to prevent inverter timeout)
- Bus conditioning read before writes when idle > 30 seconds (wakes gateway bridge)
- Dual TCP connections: separate read/write to prevent queue blocking

### USB WattNode Registers (RTU)

| Register | Description |
|----------|-------------|
| 1009 | Phase A (L1) Power — Float32 (watts) |
| 1011 | Phase B (L2) Power — Float32 (watts) |
| 1017 | Phase A Voltage |
| 1019 | Phase B Voltage |

## Recovery & Self-Healing

- **Modbus exception 3** (gateway lost XanBus): tracks consecutive failures, auto-reboots gateway after 5
- **EPC stuck** (writes accepted but inverters not responding): stop → reboot gateway → wait 90s → stop → start. Retries 3 times with 10-minute gaps, then alerts via Telegram
- **WattNode failure**: 5 consecutive read failures → SAFE mode (idle all). Auto-recovers when reads succeed
- **Gateway reboot**: via HTTPS REST API (login → OTK → reboot command)
- **Inverter cycle**: standby/operating cycle via gateway REST API to reset EPC state machine

## Telegram Bot

### Commands

| Command | Action |
|---------|--------|
| `/status` | Current state, grid power, per-inverter SOC and power, uptime |
| `/stop` | Idle all inverters (manual) |
| `/start` | Resume normal operation |
| `/up` | Increase charge/discharge +300W/inv |
| `/down` | Decrease charge/discharge -300W/inv |
| `/charge` | Force charge mode |
| `/discharge` | Force discharge mode |
| `/stats` | Session statistics (energy, ramps, guard events) |
| `/reboot` | Reboot Insight gateway |
| `/cycle` | Standby/operating cycle all inverters |
| `/help` | List all commands |

### Alerts

- Mode changes (DAY_CHARGE, NIGHT_DISCHARGE, NIGHT_REDUCED)
- Failures (SAFE mode, EPC stuck, gateway reboot failures)
- Recovery notifications
- Rate limited: one alert per type per hour

## Project Structure

```
solarcontrol/
├── main.go                  # Entry point, config, initialization
├── config.go                # YAML config structs
├── config.yaml              # Runtime configuration
├── Makefile                 # Build targets (x86, ARM64, deploy)
├── wattnode/
│   ├── reader.go            # USB Modbus RTU reads (10s loop)
│   └── registers.go         # WattNode register map, GridPower type
├── insight/
│   ├── client.go            # Modbus TCP dual-port, bus conditioning
│   ├── epc.go               # Charge/discharge/idle commands
│   ├── soc.go               # BMS bulk reads (SOC + power)
│   └── gateway.go           # HTTPS API: reboot, inverter cycle
├── controller/
│   ├── controller.go        # Main control loop, state logic
│   ├── state.go             # State machine (6 states)
│   ├── scheduler.go         # Time-based transitions
│   ├── stats.go             # Session stats, rolling averages
│   └── telegram_handler.go  # Command handlers, alert dispatch
├── telegram/
│   ├── bot.go               # Long-poll loop, message dispatch
│   └── alerts.go            # Alert/status formatting
└── deploy/
    ├── deploy.sh            # SSH deployment script
    ├── solarcontrol.service # Systemd unit
    └── solarcontrol.logrotate
```

## Build & Deploy

```bash
# Build for local testing
make build

# Build for Raspberry Pi (ARM64)
make build-arm

# Deploy to Pi (builds, copies, restarts service)
make deploy

# Or manually:
GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=$(git describe --tags)" -o solarcontrol .
scp solarcontrol nodered:/tmp/
ssh nodered "sudo mv /tmp/solarcontrol /usr/local/bin/ && sudo systemctl restart solarcontrol"
```

### Quick Commands

```bash
make logs      # Tail live logs on Pi
make status    # Check systemd status
make restart   # Restart service
make stop      # Stop service
```

## Configuration

See `config.yaml` for all options. Key settings:

```yaml
wattnode:
  port: /dev/ttyUSB0         # USB WattNode serial device
  baud: 19200
  read_interval_sec: 10      # Grid read frequency
  scale_factor: 0.5          # Hardware correction

insight:
  host: 192.168.86.86        # Insight gateway
  read_port: 503             # BMS reads
  write_port: 502            # EPC commands
  min_gap_ms: 2000           # Schneider 2-second requirement

charge:
  start_hour: 9              # Charge window: 9am-3pm
  end_hour: 15
  max_per_inverter_w: 4000   # 16kW total (80A breaker)
  dead_band_export_w: 1000   # No action 0-1kW export
  dead_band_import_w: 2000   # No action 0-2kW import

discharge:
  per_inverter_w: 600        # 2.4kW total base rate

night_guard:
  leg_export_threshold_w: 0  # Zero tolerance
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/goburrow/modbus` | Modbus RTU/TCP client |
| `github.com/goburrow/serial` | Serial port (via modbus) |
| `gopkg.in/yaml.v3` | YAML config parsing |

Standard library only for everything else (net/http for Telegram, crypto/tls for gateway HTTPS, log/slog for structured logging).

## Systemd

```ini
[Service]
Type=simple
User=root                    # Required for /dev/ttyUSB0
ExecStart=/usr/local/bin/solarcontrol -config /etc/solarcontrol/config.yaml
Restart=always
RestartSec=1                 # Instant restart on crash
```

Logs to `/var/log/solarcontrol.log` with daily rotation, 7-day retention.

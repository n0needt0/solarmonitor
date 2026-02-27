# SolarControl Go Service

## Purpose

Manage 4× XW Pro 6848 inverters to charge batteries during sun hours (ramping up to 16kW via 80A breaker based on available export) and discharge at a fixed static rate overnight. The Go service does not manage SOC — BMS and XW Pro firmware handle charge/discharge limits. The service's job is: ramp charge with the sun, set static discharge at night, monitor per-leg grid power, prevent either leg from exporting at night.

Telegram provides: status queries, manual stop/start, and alerts on mode changes or failures.

## Loads and Production (Projected)

| Load | Draw |
|------|------|
| House + pool + jacuzzi | 3.5kW constant |
| EV charging (Tesla 100D) | ~20 kWh/day avg |
| **Daily consumption** | **~104 kWh** |

| Production | Output |
|------------|--------|
| Combined arrays | ~19.5kW peak |
| **Daily (summer, 9h)** | **~100 kWh** |

These are estimates. Actual numbers will be baselined during Phase 1 (read-only). Something always eats the energy. Batteries time-shift what's left.

## Dead Bands

Dead bands prevent unnecessary Modbus writes to Insight. If the grid reading falls inside the band, no write.

**Day charge (10-min hold between adjustments):**

| Grid Reading | Action |
|-------------|--------|
| Exporting > 1kW | Ramp up charge (headroom available) |
| Exporting 0-1kW | **Dead band — no write** |
| Importing 0-2kW | **Dead band — no write** |
| Importing > 2kW | Trim charge (pulling too much from grid) |
| Batteries full, still exporting | Telegram alert once per day |

**Night discharge (zero tolerance):**

| Grid Reading | Action |
|-------------|--------|
| Either leg < 0W (any export) | Idle inverters until export stops, lock (no resume), keep monitoring |
| Importing (any amount) | No action, no write |

## Credentials

| System | Username | Password |
|--------|----------|----------|
| Insight Gateway Web UI | Admin | Admin.1234 |
| Gateway API (HTTPS) | https://192.168.86.86 | POST /auth username=Admin&password=Admin.1234 |

## System

| Component | Spec |
|-----------|------|
| Inverters | 4× XW Pro 6848, unit IDs 10 (master), 11, 13, 12 (slaves) |
| Battery | 4× SunVault, 78 kWh total, ~58 kWh usable |
| Charge breaker | 80A → max 16kW |
| Discharge rate | 2.4kW static (600W × 4) |
| Insight Facility | 192.168.86.86, ports 502 (write), 503 (read) |
| New WattNode | WND-WR-MB via USB (/dev/ttyUSB0) — all grid monitoring |
| Existing WattNode | S/N 9308266 on Insight — native fallback |

## Architecture

```
                    USB RS-485 (/dev/ttyUSB0)
New WattNode WND-WR-MB ──────────────── Raspberry Pi
(dedicated to Pi)                           │
  │                                         │  Go binary (systemd)
  │ CTs on mains (L1 + L2)                │
  │                                         ├── Modbus TCP → Insight (rare writes)
  │                                         └── HTTPS ↔ Telegram Bot API
  │
  │                                  Insight Facility
  │                                  192.168.86.86
  │                                      │ Xanbus
  │                                      ▼
  │                            ┌───────────────────┐
  │                            │  4× XW Pro 6848   │
  │                            │  Unit IDs 10-13   │
  │                            └───────────────────┘
  │
Existing WattNode (S/N 9308266)
  └── RS-485 to Insight (native fallback)
```

## Realistic Battery Cycling

The batteries never fully charge or fully deplete. In practice the daily cycle looks like:

```
Evening start:  ~85% SOC
Overnight drain: 2.4kW × ~18h = 43 kWh
Morning low:    ~30-40% SOC
Daytime charge: ramps with solar, refills
Afternoon:      back to ~85%
Repeat
```

The system cycles roughly 45-55% of capacity daily, floating in the middle of the SOC range. Never hitting the floor, never hitting the ceiling. House always imports ~1.1kW from grid overnight (3.5kW load minus 2.4kW discharge) — totally normal looking. Per-leg guard ensures neither leg exports even under uneven load. This is the gentlest possible treatment for LFP cells — shallow cycling could double the cycle life (8,000-12,000 cycles vs 4,000-6,000 at 80% DOD).

Even worst case — short winter day, partial charge — the batteries last through the night:

| Starting SOC | Usable to 15% | Hours at 2.4kW | Depletes |
|-------------|---------------|----------------|----------|
| 90% | 58.5 kWh | 24.4h | never |
| 85% | 54.6 kWh | 22.8h | never |
| 80% | 50.7 kWh | 21.1h | never |
| 70% | 42.9 kWh | 17.9h | never |

At 2.4kW discharge, batteries never run out before the next charge window regardless of starting SOC. Even at 70% start, they last 18 hours — well past the 18-hour winter night.

## What the Service Does NOT Do

- **No SOC management.** BMS protects cells. XW Pro enforces voltage limits. Batteries full → XW Pro stops charging. Batteries empty → XW Pro stops discharging. In practice neither limit is reached — the system floats in the middle of the SOC range day after day.
- **No SOC balancing.** All 4 inverters charge/discharge at equal rates. Master (unit 10) maintains ~5% higher SOC than slaves by hardware design — no software intervention needed. Future: may add logic to charge lower SOC banks first.
- **No dynamic discharge rate.** Static 600W base. Night guard idles individual inverters if any leg exports — one write per inverter, locked for rest of night. No modulation, no hunting.
- **Aggressive charge ramp.** Starts at 600W/inv, takes all available export every 10 minutes. Reaches full charge rate in ~6 writes. Trims gently with 500W buffer if importing >2kW. 3kW dead band in the middle kills cloud noise.

SOC is read for `/status` response only.

---

## State Machine

```
States: DAY_CHARGE, NIGHT_DISCHARGE, NIGHT_REDUCED, STOPPED, SAFE

DAY_CHARGE
  entry:  wait for export > 1kW, then write 600W/inv charge → Telegram: mode change
  monitor: USB WattNode every 10s, adjust every 10 min
  guard:  export > 1kW → ramp up. import > 2kW → trim. 0-1kW export / 0-2kW import → do nothing.
  writes: one adjustment per 10 min max + keepalive every 60s
  transitions:
    night_start_time    → NIGHT_DISCHARGE
    USB read fail × 5   → SAFE
    /stop command        → STOPPED

NIGHT_DISCHARGE
  entry:  write discharge 600W to all 4 inverters (once) → Telegram: mode change
  monitor: USB WattNode L1 + L2 every 10s
  guard:  ZERO tolerance — any export on any leg → idle inverters
  transitions:
    any leg < 0W         → NIGHT_REDUCED
    charge_start_time    → DAY_CHARGE (resets to waiting for export > 1kW)
    USB read fail × 5    → SAFE
    /stop command         → STOPPED

NIGHT_REDUCED
  entry:  idle inverters in single pass until no leg exports → Telegram alert
  locked: never resume idled inverters — only direction is down
  monitor: USB WattNode L1 + L2 every 10s — if export returns, idle more
  transitions:
    any leg < 0W again   → idle next inverter(s), verify, alert
    all 4 idle            → hard stop, fully locked
    charge_start_time     → DAY_CHARGE (resets to base 600W × 4)
    /stop command          → STOPPED

STOPPED
  entry:  write idle to all inverters (once) → Telegram: mode change
  monitor: USB WattNode every 10s (keep logging)
  writes: none
  transitions:
    /start command → evaluate time → DAY_CHARGE or NIGHT_DISCHARGE

SAFE
  entry:  write idle to all inverters → Telegram: failure alert
  monitor: attempt USB reads
  transitions:
    reads recover → return to previous mode → Telegram: mode change
    /stop command → STOPPED
```

### Night Discharge Guard

**Zero tolerance. Any export on any leg = immediate action.**

Each inverter contributes 300W per leg at 600W discharge. Idling one inverter removes 300W from each leg. Idle slaves first (13 → 12 → 11), master last (10).

```
Every 10s during NIGHT_DISCHARGE or NIGHT_REDUCED:
  l1 = WattNode register 1009
  l2 = WattNode register 1011

  if l1 < 0 OR l2 < 0:
    for each inverter in idle_order (skipping already-idled):
      → idle inverter (write via Insight, 2s gap)
      → verify: read L1, L2 via USB WattNode
      → if both legs ≥ 0 → stop, enter/stay NIGHT_REDUCED
      → if still exporting → continue to next inverter
    if all idle and still exporting → hard stop
    Telegram alert (once per reduction event)
```

NIGHT_REDUCED keeps monitoring. If loads shift again and export resumes, the same logic fires — idles the next available inverter(s). Only direction is down. Never resumes. Resets to base 600W × 4 at next charge window.

**Example: two separate events in one night:**

```
22:15  L1 = -500W → idle 13, verify → idle 12, verify → L1 = +100W
       Alert: "NIGHT_REDUCED: 2 of 4 (idled 13, 12)"
       
01:30  L2 = -150W → idle 11, verify → L2 = +150W  
       Alert: "NIGHT_REDUCED: 1 of 4 (idled 13, 12, 11)"

05:00  still 1 inverter running, no export
09:00  charge window → reset to 4 × 600W
```

Backoff by inverter count:

| Inverters Running | Total Discharge | Per Leg | Insight Ops |
|-------------------|-----------------|---------|-------------|
| 4 (base) | 2400W | 1200W | — |
| 3 (idle 13) | 1800W | 900W | 2 (write + verify) |
| 2 (idle 13, 12) | 1200W | 600W | 4 |
| 1 (idle 13, 12, 11) | 600W | 300W | 6 |
| 0 (hard stop) | 0W | 0W | 8 |

Max 8 ops (16 seconds on bus). Then locked — keepalives only for the rest of the night.

### Day Charge Guard

**Start low, ramp with the sun, 10-minute hold between adjustments.**

Dead band (no-write zone): exporting 0-1kW or importing 0-2kW. That's a 3kW window where the service does nothing. Most of the day sits here once ramped up.

```
Charge window opens (09:00):
  read grid every 10s, wait
  when export > 1kW sustained → set 600W per inverter (2400W), hold 10 min

Every 10 minutes:
  read grid (l1 + l2 total)

  if exporting > 1kW:
    new_total = current_total + export   # take all available export
    cap at 16000W (80A breaker)
    new_per_inv = new_total / 4
    write → verify → hold 10 min

  if importing > 2kW:
    overshoot = import - 500W      # trim back but keep 500W buffer
    new_total = current_total - overshoot
    if new_total < 0 → idle all
    new_per_inv = new_total / 4
    write → verify → hold 10 min

  if exporting 0-1kW or importing 0-2kW:
    do nothing — in the sweet spot

  if batteries full (XW Pro stops charging) and still exporting:
    Telegram once: "Batteries full. Exporting X.XkW at HH:MM"
    flag set — no further alerts until next day's charge window resets it
```

Aggressive up, gentle down. Ramp-up takes all available export in one bite — lands near the dead band in one write. Trim-down leaves a 500W buffer so you don't oscillate back into ramp territory.

**Morning ramp example (clear day):**

```
09:00  export 0.5kW          → wait (under 1kW threshold)
09:05  export 1.2kW          → start 600W/inv (2400W). Hold 10 min.
09:15  export 3.0kW          → add 3.0kW → 5400W (1350/inv). Hold.
09:25  export 4.5kW          → add 4.5kW → 9900W (2475/inv). Hold.
09:35  export 2.0kW          → add 2.0kW → 11900W (2975/inv). Hold.
09:45  export 1.5kW          → add 1.5kW → 13400W (3350/inv). Hold.
09:55  export 0.6kW          → dead band, done.
```

6 writes, at full charge by 10am.

**Cloudy day:**

```
09:15  start at 2400W
09:25  cloud → import 2.5kW  → trim 2kW → 400W/inv. Hold.
09:35  sun → export 3kW      → add 3kW → 4400W. Hold.
09:45  cloud → import 1.5kW  → in dead band, do nothing
```

3 writes. The 3kW dead band absorbs cloud transients.

**Write budget during day charge:**

| Condition | Writes/Day |
|-----------|-----------|
| Clear morning ramp | ~6 |
| Stable at full charge | 0 |
| Cloudy/variable | ~15 |
| Keepalives | ~360 |
| **Worst case total** | **~375** |

Insight at <1% utilization during charge.

---

## Telegram Integration

Bidirectional. The Go service polls `getUpdates` on a loop. Only responds to the configured chat_id.

### Incoming Commands

| Command | Action | Response |
|---------|--------|----------|
| `/status` | Read current state | State, grid L1/L2, SOC, charge/discharge rate, uptime |
| `/stop` | Idle all inverters, enter STOPPED | State snapshot |
| `/start` | Resume based on time of day | State snapshot |
| `/up` | Increase current rate by 300W per inverter | State snapshot with new rate |
| `/down` | Decrease current rate by 300W per inverter | State snapshot with new rate |

`/up` and `/down` adjust whatever the current operation is — charge or discharge. If at max (4000W/inv charge = 16kW total) or min (0W), command is ignored with a message. During NIGHT_REDUCED, `/up` is rejected — only down allowed. `/stop` requires manual `/start` to resume — no auto-resume.

### `/status` Response

```
State:       DAY_CHARGE
Grid:        exporting 0.6kW (dead band)
L1:          +320W  L2: +280W
Charge:      2475W/inv (9900W total)
SOC:         72%
Uptime:      6h 15m
Last alert:  DAY_CHARGE at 09:15
```

### Outgoing Alerts

Each alert type fires **once per day max**. Every alert includes a state snapshot: mode, grid import/export, SOC.

| Alert | Fires | Resets |
|-------|-------|--------|
| Mode change (day→night, night→day) | Once per transition | Next transition |
| Night reduced (leg export) | Once per reduction event | Never (can only go down) |
| Batteries full | Once per day | Next charge window |
| Start/stop (manual) | Once per command | Next command |
| Failure (SAFE mode) | Once per failure | Recovery |

**Alert format:**

```
→ DAY_CHARGE at 09:15
  Grid: exporting 1.2kW
  SOC: 38%
  Charge: 600W/inv (starting)

→ NIGHT_DISCHARGE at 15:00
  Grid: importing 1.1kW
  SOC: 83%
  Discharge: 600W × 4

→ NIGHT_REDUCED at 22:15
  Grid: L1 -500W L2 +2000W → L1 +100W L2 +1400W
  SOC: 45%
  Inverters: 2 of 4 active (idled 13, 12)

→ NIGHT_REDUCED at 01:30 (further reduction)
  Grid: L1 +800W L2 -150W → L1 +500W L2 +150W
  SOC: 32%
  Inverters: 1 of 4 active (idled 13, 12, 11)

→ BATTERIES FULL at 14:15
  Grid: exporting 3.8kW
  SOC: 100%
  Charge: stopped (XW Pro)

→ STOPPED at 11:30 (manual)
  Grid: exporting 2.1kW
  SOC: 72%
  Inverters: all idle

→ STARTED at 11:35 (manual)
  Grid: exporting 2.1kW
  SOC: 72%
  Mode: DAY_CHARGE

❌ SAFE at 03:22
  Last grid: L1 620W L2 580W
  Last SOC: 61%
  Reason: WattNode read failed × 5

✅ RECOVERED at 03:24
  Grid: L1 620W L2 580W
  SOC: 61%
  Resuming: NIGHT_DISCHARGE
```

### Implementation

Poll `getUpdates` with long polling (timeout 30s) in a goroutine. Parse commands. Send via `sendMessage`. Only accept messages from configured chat_id — ignore everything else.

```go
// Poll loop (separate goroutine)
for {
    updates = GET /getUpdates?offset=X&timeout=30
    for each update:
        if update.chat_id != config.chat_id: skip
        switch update.text:
            "/status" → reply with current state
            "/stop"   → signal controller → STOPPED
            "/start"  → signal controller → evaluate time → resume
            "/up"     → signal controller → +300W/inv → reply with new state
            "/down"   → signal controller → -300W/inv → reply with new state
}

// Alert sending (called from state machine on transitions)
func alert(msg string) {
    POST /sendMessage {chat_id, text: msg}
}
```

---

## Write Minimization

USB WattNode reads grid power locally every 10 seconds. Insight is only touched for:

1. **Mode change** — 1-4 writes (twice per day + rare throttle events)
2. **Keepalive** — 1 write per 60 seconds
3. **SOC read** — 1 read per 60 seconds (for /status only)

### Write Decision Logic

```
Every 10 seconds (LOCAL USB only):
  l1, l2 = read WattNode registers 1009, 1011
  total = l1 + l2

Every 60 seconds (INSIGHT):
  soc = read port 503 (for /status response only)
  send keepalive (resend last command to active inverters)

Every 10 minutes during DAY_CHARGE (if not in hold):
  if total export > 1kW → calculate new charge rate, write, hold 10 min
  if total import > 2kW → calculate trimmed rate, write, hold 10 min
  if between → do nothing

Write to Insight ONLY when:
  charge window opens + export > 1kW → write 600W/inv starting charge
  10 min check: export > 1kW         → ramp up
  10 min check: import > 2kW         → trim down
  night_start_time reached            → write discharge 600W to all 4
  night: any leg < 0                  → idle inverter(s) until export stops, keep monitoring
  /stop received                      → write idle to all
  /start received                     → write charge or discharge
  /up received                        → write current rate + 300W/inv
  /down received                      → write current rate - 300W/inv
  USB read fail × 5                   → write idle to all

NEVER write for:
  day: exporting 0-1kW
  day: importing 0-2kW
  day: during 10 min hold
  night: importing (any amount)
  SOC changes
```

### Insight Traffic

~2,900 ops/day (mostly keepalives + SOC reads). Insight capacity: 43,200 ops/day. Uses ~7%.

---

## Key Constraints

- **2-second gap** between Insight Modbus TCP ops
- **Schneider wake-up**: first command after idle may be ignored — send read first
- **EPC = upper limit**: inverter delivers up to commanded rate, not exactly it
- **Byte encoding**: WattNode Float32 — Big Endian bytes, Little Endian word order
- **Heartbeat**: XW Pros revert if EPC stops — keepalive every 60s
- **80A breaker**: hardware caps charge at 16kW

## Startup Behavior

1. Stop Node-RED (if running) — cannot have two controllers
2. Service starts in IDLE state
3. Write idle (EPC=0) to all 4 inverters — ensure clean slate
4. Read current time, evaluate which state to enter
5. Time controller takes over — transitions to DAY_CHARGE or NIGHT_DISCHARGE based on schedule
6. Telegram alert: "STARTED — entering [state]"

On crash/restart: systemd restarts within 1 second, startup sequence repeats.

## Graceful Degradation

- USB WattNode fails × 5 → SAFE (idle all) + Telegram failure alert. Native Grid Support active.
- Insight TCP fails → retry with backoff. Inverters hold last command until heartbeat timeout.
- Process dies → systemd restarts <1 second.

---

## Config

```yaml
wattnode:
  port: /dev/ttyUSB0
  baud: 9600
  read_interval_sec: 10

insight:
  host: 192.168.86.86
  min_gap_ms: 2000
  soc_read_interval_sec: 60
  keepalive_sec: 60

inverters:
  master_unit_id: 10
  slave_unit_ids: [11, 13, 12]  # Physical XanBus order
  write_mode: all              # write to all 4 inverters

charge:
  start_time: "09:00"        # winter (Nov-May): 9am-3pm window
  end_time: "15:00"          # winter: 3pm
  start_per_inverter_w: 600  # initial charge rate
  max_per_inverter_w: 4000   # hardware max per inverter
  max_total_w: 16000         # 80A breaker cap (4000W × 4)
  export_start_w: 1000       # start charging when export > this
  export_ramp_w: 1000        # ramp up when export > this (takes all available)
  import_trim_w: 2000        # trim down when import > this
  trim_buffer_w: 500         # keep 500W import buffer on trim-down only
  hold_sec: 600              # 10 minute hold between adjustments

discharge:
  per_inverter_w: 600        # 2.4kW total, base rate
  start_time: "15:00"        # winter: 3pm
  idle_order: [12, 13, 11, 10]  # slaves first (reverse XanBus order), master last

night_guard:
  leg_export_threshold_w: 0  # any export = act immediately
  resume_allowed: false      # never resume idled inverters, only idle more

safety:
  max_read_failures: 5

startup:
  initial_state: IDLE        # Start idle, time controller takes over
  zero_all_on_start: true    # Write idle to all inverters on startup

logging:
  retention_days: 7
  use_logrotate: true

telegram:
  bot_token: ""
  chat_id: ""
  poll_timeout_sec: 30
  manual_step_w: 300         # /up and /down adjust by this per inverter
```

---

## Project Structure

```
solarcontrol/
├── main.go
├── config.go
├── config.yaml
├── wattnode/
│   ├── reader.go        # USB Modbus RTU reads (10-sec loop)
│   └── registers.go     # Register addresses, Float32 parsing
├── insight/
│   ├── client.go        # Modbus TCP, 2-sec gating
│   ├── epc.go           # Charge/discharge/idle commands
│   └── soc.go           # SOC reads for /status
├── controller/
│   ├── state.go         # State machine: DAY_CHARGE, NIGHT, NIGHT_REDUCED, STOPPED, SAFE
│   ├── scheduler.go     # Time-based transitions
│   ├── charge_ramp.go   # 10-min ramp/trim logic, dead band, hold timer
│   ├── night_guard.go   # Zero-tolerance per-leg export → idle inverters, lock
│   └── queue.go         # Insight Modbus write queue, 2-sec drain, priority
├── telegram/
│   ├── bot.go           # Long-poll getUpdates loop
│   ├── commands.go      # /status, /stop, /start, /up, /down handlers
│   └── alerts.go        # Mode change + failure alerts
├── health/
│   └── watchdog.go      # systemd notify
└── go.mod
```

## Systemd

```ini
[Unit]
Description=SolarControl
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/solarcontrol
Restart=always
RestartSec=1
WatchdogSec=30

[Install]
WantedBy=multi-user.target
```

## Logrotate

```
/var/log/solarcontrol.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
```

## Logging

```
level=INFO msg="day_charge_start" per_inv_w=600 total_w=2400 export_w=1200
level=INFO msg="day_charge_ramp" per_inv_w=2475 total_w=9900 export_w=4500
level=INFO msg="day_charge_trim" per_inv_w=1400 total_w=5600 import_w=2500
level=INFO msg="day_charge_hold" reason="dead_band" export_w=800
level=INFO msg="batteries_full" export_w=3800
level=INFO msg="night_mode" discharge_w=600 inverters=4
level=INFO msg="read" l1_w=1234 l2_w=568 total_w=1802 src=usb
level=INFO msg="soc" pct=72 src=insight
level=INFO msg="keepalive" unit=10 mode=charge
level=INFO msg="night_export" l1_w=-500 l2_w=2000 action=idle_inverter unit=13
level=INFO msg="night_export" l1_w=-200 l2_w=1400 action=idle_inverter unit=12
level=INFO msg="night_reduced" active=2 idled="13,12" reason="leg_export"
level=INFO msg="night_export" l2_w=-150 l1_w=800 action=idle_inverter unit=11
level=INFO msg="night_reduced" active=1 idled="13,12,11" reason="leg_export"
level=INFO msg="telegram_cmd" cmd=status from=chat_id
level=INFO msg="telegram_cmd" cmd=up from=chat_id per_inv_w=900 total_w=3600
level=INFO msg="telegram_cmd" cmd=down from=chat_id per_inv_w=300 total_w=1200
level=INFO msg="telegram_alert" type=mode_change state=NIGHT_REDUCED
level=ERROR msg="read_fail" source=wattnode consecutive=3
level=ERROR msg="insight_fail" error="connection reset" retry=2
```

## Build and Deploy

```bash
GOOS=linux GOARCH=arm64 go build -o solarcontrol .
scp solarcontrol nodered:/usr/local/bin/
ssh nodered "sudo systemctl stop nodered && sudo systemctl disable nodered"
ssh nodered "sudo systemctl enable solarcontrol && sudo systemctl start solarcontrol"
```

**Important:** Stop Node-RED before starting solarcontrol — both manage the same inverters via Modbus. Running both simultaneously will cause conflicts.

---

## Modbus Registers

### WattNode (RTU, USB)

| Register | Description | Type |
|----------|-------------|------|
| 1009 | Phase A Power (W) | Float32 (BE bytes, LE words) |
| 1011 | Phase B Power (W) | Float32 |
| 1015 | Total Power (W) | Float32 |
| 1017 | Phase A Voltage | Float32 |
| 1019 | Phase B Voltage | Float32 |

### XW Pro — Port 502 (Write)

| Register | Description |
|----------|-------------|
| 40210 | EPC Charge Max Power (watts) |
| 40213 | EPC Mode Command: 0=idle, 1=charge |
| 40152 | EPC Max Discharge Power (watts) |
| 40149 | Recharge SOC (% × 10, e.g., 990 = 99%) |

### XW Pro — Port 503 (Read)

| Register | Description |
|----------|-------------|
| 40210 | Current EPC Charge Limit (watts) |
| 40213 | Current EPC Mode (0=idle, 1=charge) |

### BMS Bulk Read — Port 503, Unit ID 1 (Registers 960-1001)

| Offset | Description |
|--------|-------------|
| 7, 9 | Inv 1: SOC (%), Battery Power (W, signed) |
| 17, 19 | Inv 2: SOC (%), Battery Power (W) |
| 27, 29 | Inv 3: SOC (%), Battery Power (W) |
| 37, 39 | Inv 4: SOC (%), Battery Power (W) |

Note: Battery power values >32767 are negative (subtract 65536).

---

## References

- [WattNode Manual](https://www.instrumentation2000.com/pub/media/pdf/ccs-wnd-wr-mb-manual.pdf)
- [WattNode Register Map](https://ctlsys.com/support/wattnode-modbus-register-map/)
- [Schneider Modbus Maps](https://solar.se.com/us/wp-content/uploads/sites/7/2022/02/Conext-Gateway-InsightHome-InsightFacility-Modbus-Maps.zip)
- [goburrow/modbus](https://github.com/goburrow/modbus)
- [DIY Solar — XW Pro Modbus](https://diysolarforum.com/threads/schneider-xw-pro-modbus.94314/)
- [Beene Brothers Node-RED](https://diysolarforum.com/threads/adventures-of-zero-consumption-schneider-bcs-setup-ac-coupled.110526/)

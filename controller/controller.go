package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/n0needt0/solarcontrol/insight"
	"github.com/n0needt0/solarcontrol/wattnode"
)

// Alerter sends notifications on state changes
type Alerter interface {
	SendModeChangeAlert(state string, detail string)
	SendFailureAlert(reason string)
	SendRecoveryAlert(state string)
}

// Controller orchestrates the solar charge control logic
type Controller struct {
	cfg *Config

	// Communication
	insight  *insight.Client
	wattnode *wattnode.Reader
	rebooter *insight.GatewayRebooter

	// State management
	state     *StateManager
	scheduler *Scheduler
	alerter   Alerter

	// Current readings
	mu          sync.RWMutex
	gridPower   wattnode.GridPower
	bmsStatus   *insight.BatteryStatus
	lastGridAt  time.Time
	lastBMSAt   time.Time

	// Charge tracking
	currentChargeW    int  // Current charge limit per inverter
	waitingForExport  bool // True when waiting for export > threshold to start charging
	lastRampAt        time.Time
	lastKeepaliveAt   time.Time
	consecutiveFail   int
	starvationAt      time.Time // When we first saw low power at floor rate
	consecutiveWriteFail int       // Consecutive Modbus write failures (exception 3 = bus lost)
	lastRebootAt         time.Time // Last gateway reboot attempt
	lastCycleAt          time.Time // Last inverter standby cycle attempt
	epcStuckSince        time.Time // When we first detected EPC stuck (writes OK but BMS power 0)
	epcCycleCount        int       // Consecutive standby cycles that failed to unstick EPC

	// Day peak shave tracking
	dayDischargeSince time.Time // When sustained high import started during day
	dayDischarging    bool      // Currently peak shaving (discharging during day)

	// Manual override: skip time-based transitions until window boundary or /start /stop
	manualOverride      bool
	manualOverrideInDay bool // true if override was set during charge window
	manualStopped       bool // true if stopped by /stop command (don't auto-resume)

	// Night discharge tracking
	currentDischargeW   int       // Current discharge rate per inverter
	dischargeRampSince  time.Time // When we first saw high import for discharge ramp
	lastDischargeAdjust time.Time // Last time we adjusted discharge rate

	// Session statistics
	stats *StatsTracker

	// Control
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Config holds controller configuration
type Config struct {
	// Inverter unit IDs
	MasterUnitID byte
	SlaveUnitIDs []byte
	IdleOrder    []byte

	// Charge window
	ChargeStartHour int
	ChargeEndHour   int

	// Charge limits
	StartPerInvW    int
	MaxPerInvW      int
	MaxTotalW       int
	ExportStartW    int
	RampUpHoldSec   int
	RampDownHoldSec int
	DeadBandExportW int
	DeadBandImportW int

	// Discharge
	DischargePerInvW    int
	MaxDischargePerInvW int

	// Night guard
	LegExportThresholdW int
	ResumeAllowed       bool

	// Safety
	MaxReadFailures int

	// Intervals
	GridReadInterval time.Duration
	BMSReadInterval  time.Duration
}

// AllUnitIDs returns all inverter unit IDs (master first)
func (c *Config) AllUnitIDs() []byte {
	ids := make([]byte, 0, 4)
	ids = append(ids, c.MasterUnitID)
	ids = append(ids, c.SlaveUnitIDs...)
	return ids
}

// New creates a new controller
func New(cfg *Config, ins *insight.Client, wn *wattnode.Reader) *Controller {
	return &Controller{
		cfg:       cfg,
		insight:   ins,
		wattnode:  wn,
		state:     NewStateManager(),
		scheduler: NewScheduler(cfg.ChargeStartHour, cfg.ChargeEndHour),
		stats:     NewStatsTracker(cfg.MaxPerInvW),
		stopCh:    make(chan struct{}),
	}
}

// SetAlerter sets the notification handler for state changes
func (c *Controller) SetAlerter(a Alerter) {
	c.alerter = a
}

// SetRebooter sets the gateway rebooter for automatic recovery
func (c *Controller) SetRebooter(r *insight.GatewayRebooter) {
	c.rebooter = r
}

// Stats returns the session stats tracker
func (c *Controller) Stats() *StatsTracker {
	return c.stats
}

// currentSOC returns the current average SOC, or 0 if BMS data is unavailable
func (c *Controller) currentSOC() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.bmsStatus != nil {
		return c.bmsStatus.TotalSOC()
	}
	return 0
}

// Start begins the control loop
func (c *Controller) Start(ctx context.Context) error {
	slog.Info("controller starting",
		"charge_window", c.scheduler.ChargeWindowString(),
		"master_id", c.cfg.MasterUnitID,
		"slave_ids", c.cfg.SlaveUnitIDs,
	)

	// Initial state based on time
	desiredState := c.scheduler.DesiredState()
	if desiredState == StateDayCharge {
		c.stats.StartSession(SessionCharge, 0)
		c.state.Transition(StateDayCharge, "startup in charge window")
	} else {
		c.stats.StartSession(SessionDischarge, 0)
		c.state.Transition(StateNightDischarge, "startup in discharge window")
	}

	// Apply initial configuration
	if err := c.applyCurrentState(); err != nil {
		slog.Error("failed to apply initial state", "error", err)
		// Continue anyway - will retry
	}

	// Start background loops
	c.wg.Add(3)
	go c.gridReadLoop(ctx)
	go c.controlLoop(ctx)
	go c.alertLoop(ctx)

	return nil
}

// Stop stops the controller gracefully
func (c *Controller) Stop() {
	slog.Info("controller stopping")
	close(c.stopCh)
	c.wg.Wait()

	// Set all inverters to idle on shutdown
	if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
		slog.Error("failed to idle inverters on shutdown", "error", err)
	}

	slog.Info("controller stopped")
}

// gridReadLoop reads WattNode every GridReadInterval
func (c *Controller) gridReadLoop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.GridReadInterval)
	defer ticker.Stop()

	// Read immediately on start
	c.readGrid()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.readGrid()
		}
	}
}

// readGrid performs a WattNode read and updates state
func (c *Controller) readGrid() {
	power, err := c.wattnode.Read()
	if err != nil {
		c.consecutiveFail++
		slog.Warn("wattnode read failed",
			"error", err,
			"consecutive_failures", c.consecutiveFail,
		)

		if c.consecutiveFail >= c.cfg.MaxReadFailures {
			c.enterSafeMode("too many consecutive WattNode read failures")
		}
		return
	}

	now := time.Now()

	c.mu.Lock()
	c.gridPower = *power
	c.lastGridAt = now
	c.consecutiveFail = 0
	c.mu.Unlock()

	c.stats.RecordGridReading(power.Total, now)

	slog.Debug("grid_read",
		"l1_w", power.L1,
		"l2_w", power.L2,
		"total_w", power.Total,
	)
}

// controlLoop is the main control loop
func (c *Controller) controlLoop(ctx context.Context) {
	defer c.wg.Done()

	// Control loop runs at same interval as grid reads
	ticker := time.NewTicker(c.cfg.GridReadInterval)
	defer ticker.Stop()

	// Also read BMS periodically
	bmsTicker := time.NewTicker(c.cfg.BMSReadInterval)
	defer bmsTicker.Stop()

	// Keepalive every 60 seconds
	keepaliveTicker := time.NewTicker(60 * time.Second)
	defer keepaliveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return

		case <-bmsTicker.C:
			c.readBMS()

		case <-keepaliveTicker.C:
			c.sendKeepalive()

		case <-ticker.C:
			c.runControlCycle()
		}
	}
}

// sendKeepalive resends current command to prevent inverter timeout
func (c *Controller) sendKeepalive() {
	state := c.state.Current()

	// In SAFE mode, periodically probe to see if gateway recovered
	if state == StateSafe {
		c.probeSafeModeRecovery()
		return
	}

	// Only send keepalives in active states
	if state == StateStopped || state == StateIdle {
		return
	}

	slog.Debug("sending_keepalive", "state", state.String())

	var keepaliveErr error

	switch state {
	case StateDayCharge:
		if c.dayDischarging {
			if c.currentDischargeW > 0 {
				keepaliveErr = c.writeDischargeKeepalive(c.currentDischargeW)
				if keepaliveErr != nil {
					slog.Warn("keepalive day discharge failed", "error", keepaliveErr)
				}
			}
		} else if c.currentChargeW > 0 {
			keepaliveErr = c.writeChargeKeepalive(c.currentChargeW)
			if keepaliveErr != nil {
				slog.Warn("keepalive charge failed", "error", keepaliveErr)
			}
		}

	case StateNightDischarge, StateNightReduced:
		if c.currentDischargeW > 0 {
			keepaliveErr = c.writeDischargeKeepalive(c.currentDischargeW)
			if keepaliveErr != nil {
				slog.Warn("keepalive discharge failed", "error", keepaliveErr)
			}
		}
	}
	c.lastKeepaliveAt = time.Now()

	// Track consecutive Modbus exception 3 — indicates gateway lost Xanbus to inverters
	const maxWriteFail = 5
	if keepaliveErr != nil && insight.IsModbusException3(keepaliveErr) {
		c.consecutiveWriteFail++
		slog.Warn("modbus_exception3",
			"consecutive", c.consecutiveWriteFail,
			"max", maxWriteFail,
		)
		if c.consecutiveWriteFail >= maxWriteFail {
			// Try rebooting the gateway before entering SAFE mode
			if c.rebooter != nil {
				slog.Info("gateway_reboot_attempt", "reason", "modbus exception 3")
				if c.alerter != nil {
					c.alerter.SendFailureAlert(fmt.Sprintf("Modbus exception 3 x%d — rebooting gateway", c.consecutiveWriteFail))
				}
				rebootCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				rebootErr := c.rebooter.Reboot(rebootCtx)
				cancel()
				if rebootErr != nil {
					slog.Error("gateway_reboot_failed", "error", rebootErr)
				} else {
					slog.Info("gateway_reboot_success", "waiting", "3m for gateway to restart")
					time.Sleep(3 * time.Minute)
				}
				// Set after sleep so retry interval starts from now
				c.lastRebootAt = time.Now()
			}
			c.enterSafeMode(fmt.Sprintf("Modbus exception 3 x%d — gateway lost Xanbus to inverters", c.consecutiveWriteFail))
		}
	} else if keepaliveErr == nil {
		c.consecutiveWriteFail = 0

		// EPC stuck detection: writes succeed but BMS shows zero power.
		// This means the gateway accepts Modbus writes but the XW Pro's internal
		// EPC controller is not acting on them. Only a standby cycle fixes it.
		c.checkEPCStuck()
	}
}

// resetEPCStuck clears EPC stuck detection state
func (c *Controller) resetEPCStuck() {
	c.epcStuckSince = time.Time{}
	c.epcCycleCount = 0
}

// checkEPCStuck detects when writes succeed but inverters aren't responding.
// Recovery sequence mirrors what works manually: stop → reboot gateway → stop → start.
func (c *Controller) checkEPCStuck() {
	state := c.state.Current()

	// Only check in active charge/discharge states with a commanded rate
	var commandedRate int
	switch state {
	case StateDayCharge:
		if c.dayDischarging {
			commandedRate = c.currentDischargeW
		} else {
			commandedRate = c.currentChargeW
		}
	case StateNightDischarge, StateNightReduced:
		commandedRate = c.currentDischargeW
	default:
		c.resetEPCStuck()
		return
	}

	if commandedRate == 0 {
		c.resetEPCStuck()
		return
	}

	// Check BMS power
	c.mu.RLock()
	bms := c.bmsStatus
	bmsAge := time.Since(c.lastBMSAt)
	c.mu.RUnlock()

	if bms == nil || bmsAge > 2*time.Minute {
		return
	}

	// Skip when batteries are nearly full (charging) or nearly empty (discharging).
	avgSOC := bms.TotalSOC()
	isCharging := state == StateDayCharge && !c.dayDischarging
	if isCharging && avgSOC >= 97 {
		c.resetEPCStuck()
		return
	}
	if !isCharging && avgSOC <= 5 {
		c.resetEPCStuck()
		return
	}

	// Sum absolute power across all inverters
	absPower := 0
	for _, p := range bms.Power {
		if p < 0 {
			absPower += int(-p)
		} else {
			absPower += int(p)
		}
	}

	const stuckThresholdW = 200
	if absPower >= stuckThresholdW {
		if c.epcCycleCount > 0 {
			slog.Info("epc_unstuck", "bms_power", absPower, "after_attempts", c.epcCycleCount)
		}
		c.resetEPCStuck()
		return
	}

	now := time.Now()
	if c.epcStuckSince.IsZero() {
		c.epcStuckSince = now
		slog.Warn("epc_stuck_detected",
			"commanded_w", commandedRate,
			"bms_power", absPower,
		)
		return
	}

	stuckDuration := now.Sub(c.epcStuckSince)
	if stuckDuration < 3*time.Minute {
		return
	}

	if c.rebooter == nil {
		return
	}

	const maxAttempts = 3
	const retryInterval = 10 * time.Minute

	if c.epcCycleCount >= maxAttempts {
		// Alert once when we exhaust all attempts
		if c.epcCycleCount == maxAttempts {
			c.epcCycleCount++ // prevent repeated alerts
			slog.Error("epc_stuck_gave_up", "attempts", maxAttempts)
			if c.alerter != nil {
				c.alerter.SendFailureAlert(fmt.Sprintf(
					"EPC stuck — %d recovery attempts failed. Manual /stop + /start needed.",
					maxAttempts,
				))
			}
		}
		return
	}
	if !c.lastCycleAt.IsZero() && time.Since(c.lastCycleAt) < retryInterval {
		return
	}

	c.epcCycleCount++
	c.lastCycleAt = now

	slog.Warn("epc_stuck_recovering",
		"attempt", c.epcCycleCount,
		"max", maxAttempts,
		"stuck_for", stuckDuration,
		"commanded_w", commandedRate,
		"bms_power", absPower,
	)
	if c.alerter != nil {
		c.alerter.SendFailureAlert(fmt.Sprintf(
			"EPC stuck (attempt %d/%d) — BMS %dW, commanded %dW/inv. Running stop → reboot → start.",
			c.epcCycleCount, maxAttempts, absPower, commandedRate,
		))
	}

	// Step 1: Stop — idle all inverters
	slog.Info("epc_recovery_stop")
	c.insight.IdleAllInverters(c.cfg.AllUnitIDs())

	// Step 2: Reboot gateway
	slog.Info("epc_recovery_reboot")
	rebootCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	err := c.rebooter.Reboot(rebootCtx)
	cancel()
	c.lastRebootAt = time.Now()

	if err != nil {
		slog.Error("epc_recovery_reboot_failed", "error", err)
		// Still try to restart — gateway might come back
	} else {
		slog.Info("epc_recovery_reboot_sent", "waiting", "90s")
	}

	// Step 3: Wait for gateway to come back
	time.Sleep(90 * time.Second)

	// Step 4: Stop again (clean slate after reboot)
	slog.Info("epc_recovery_stop_after_reboot")
	c.insight.IdleAllInverters(c.cfg.AllUnitIDs())
	time.Sleep(5 * time.Second)

	// Step 5: Start — re-enter current state
	slog.Info("epc_recovery_start")
	if err := c.applyCurrentState(); err != nil {
		slog.Error("epc_recovery_apply_failed", "error", err)
	}

	c.resetEPCStuck()

	if c.alerter != nil {
		c.alerter.SendRecoveryAlert("EPC recovery complete (stop → reboot → start)")
	}
}

// probeSafeModeRecovery tries a test write to see if the gateway recovered.
// If probe fails and rebooter is available, retries gateway reboot every 10 minutes.
func (c *Controller) probeSafeModeRecovery() {
	// Try to idle unit 10 — if it works, gateway is back
	err := c.insight.SetIdleMode(c.cfg.MasterUnitID)
	if err != nil {
		slog.Debug("safe_mode_probe_failed", "error", err)

		// Retry gateway reboot every 5 minutes while in SAFE mode.
		// After a successful reboot, wait 3 min for gateway to fully restart
		// (boot + XanBus re-init) before probing or retrying.
		const rebootRetryInterval = 5 * time.Minute
		const rebootRecoveryWait = 3 * time.Minute
		if c.rebooter != nil && time.Since(c.lastRebootAt) >= rebootRetryInterval {
			slog.Info("gateway_reboot_retry", "last_attempt", c.lastRebootAt.Format("15:04:05"))
			if c.alerter != nil {
				c.alerter.SendFailureAlert("SAFE mode — retrying gateway reboot")
			}
			rebootCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			rebootErr := c.rebooter.Reboot(rebootCtx)
			cancel()
			if rebootErr != nil {
				slog.Error("gateway_reboot_retry_failed", "error", rebootErr)
			} else {
				slog.Info("gateway_reboot_retry_success", "waiting", rebootRecoveryWait)
				time.Sleep(rebootRecoveryWait)
			}
			// Set lastRebootAt after sleep so the full retry interval starts from now
			c.lastRebootAt = time.Now()
		}
		return
	}

	slog.Info("safe_mode_probe_success", "unit", c.cfg.MasterUnitID)
	c.consecutiveWriteFail = 0

	// Recover: transition to time-appropriate state
	soc := c.currentSOC()

	desiredState := c.scheduler.DesiredState()
	sessionType := SessionDischarge
	if desiredState == StateDayCharge {
		sessionType = SessionCharge
	}
	c.stats.StartSession(sessionType, soc)
	c.state.Transition(desiredState, "gateway recovered")

	if err := c.applyCurrentState(); err != nil {
		slog.Error("failed to apply state on recovery", "error", err)
	}
}

// alertLoop reads state changes and sends telegram notifications
func (c *Controller) alertLoop(ctx context.Context) {
	defer c.wg.Done()

	ch := c.state.StateChangeCh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case change := <-ch:
			if c.alerter == nil {
				continue
			}
			switch change.To {
			case StateSafe:
				c.alerter.SendFailureAlert(change.Reason)
			default:
				// Recovery: coming out of SAFE back to a normal state
				if change.From == StateSafe {
					c.alerter.SendRecoveryAlert(change.To.String())
				} else {
					c.alerter.SendModeChangeAlert(change.To.String(), change.Reason)
				}
			}
		}
	}
}

// readBMS reads battery status from all inverters
func (c *Controller) readBMS() {
	bms, err := c.insight.ReadBatteryStatus()
	if err != nil {
		slog.Warn("bms read failed", "error", err)
		return
	}

	c.mu.Lock()
	prevBMSAt := c.lastBMSAt
	c.bmsStatus = bms
	c.lastBMSAt = time.Now()
	c.mu.Unlock()

	// Integrate battery energy for stats
	if !prevBMSAt.IsZero() {
		intervalSec := time.Since(prevBMSAt).Seconds()
		c.stats.RecordChargeEnergy(bms.TotalPower(), intervalSec)
	}

	slog.Debug("bms_read",
		"soc", bms.SOC,
		"power", bms.Power,
		"total_soc", bms.TotalSOC(),
	)
}

// runControlCycle executes one control cycle
func (c *Controller) runControlCycle() {
	// Check for time-based state transitions first
	c.checkTimeTransitions()

	// Get current state and readings
	currentState := c.state.Current()

	c.mu.RLock()
	gridPower := c.gridPower
	gridAge := time.Since(c.lastGridAt)
	c.mu.RUnlock()

	// Skip if grid data is stale
	if gridAge > 2*c.cfg.GridReadInterval {
		slog.Warn("stale grid data, skipping control cycle", "age", gridAge)
		return
	}

	switch currentState {
	case StateIdle:
		// Do nothing - wait for time transition
		return

	case StateDayCharge:
		c.runDayChargeLogic(gridPower)
		c.stats.RecordMaxRateTime(c.currentChargeW, c.cfg.GridReadInterval.Seconds())

	case StateNightDischarge:
		c.runNightDischargeLogic(gridPower)

	case StateNightReduced:
		c.runNightReducedLogic(gridPower)

	case StateStopped:
		// Manual stop - do nothing
		return

	case StateSafe:
		// Safe mode - all idle, check for recovery
		return
	}
}

// checkTimeTransitions handles time-based state changes
func (c *Controller) checkTimeTransitions() {
	currentState := c.state.Current()

	// SAFE: never auto-resume
	if currentState == StateSafe {
		return
	}

	// STOPPED: auto-resume on charge window only if not manually stopped
	if currentState == StateStopped {
		if !c.manualStopped && c.scheduler.IsChargeWindow() {
			slog.Info("stopped_auto_resume", "reason", "charge window opened")
		} else {
			return
		}
	}

	// Manual override: hold until the time window actually changes from when override was set
	if c.manualOverride {
		inChargeNow := c.scheduler.IsChargeWindow()
		if inChargeNow == c.manualOverrideInDay {
			// Same window as when override was set — keep holding
			return
		}
		// Window boundary crossed — clear override and fall through to normal transition
		slog.Info("manual_override_cleared", "reason", "window boundary crossed")
		c.manualOverride = false
	}

	// Get current SOC for stats
	soc := c.currentSOC()

	// Time-based transitions
	if c.scheduler.IsChargeWindow() {
		if currentState != StateDayCharge {
			c.stats.StartSession(SessionCharge, soc)
			c.state.Transition(StateDayCharge, "entering charge window")
			c.currentChargeW = 0 // Reset charge level
			if err := c.applyCurrentState(); err != nil {
				slog.Error("failed to apply day charge state", "error", err)
			}
		}
	} else {
		if currentState != StateNightDischarge && currentState != StateNightReduced {
			const minDischargeSOC = 30
			if soc > 0 && soc < minDischargeSOC {
				slog.Info("discharge_skipped_low_soc",
					"soc", soc,
					"min", minDischargeSOC,
				)
				c.state.Transition(StateStopped, fmt.Sprintf("SOC %d%% below %d%% minimum for discharge", soc, minDischargeSOC))
				if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
					slog.Error("failed to idle inverters on low SOC skip", "error", err)
				}
			} else {
				c.stats.StartSession(SessionDischarge, soc)
				c.state.Transition(StateNightDischarge, "entering discharge window")
				if err := c.applyCurrentState(); err != nil {
					slog.Error("failed to apply night discharge state", "error", err)
				}
			}
		}
	}
}

// applyCurrentState sets inverter registers based on current state
func (c *Controller) applyCurrentState() error {
	state := c.state.Current()
	unitIDs := c.cfg.AllUnitIDs()

	slog.Info("applying_state", "state", state.String())

	switch state {
	case StateDayCharge:
		// Don't start charging immediately - wait for export > threshold
		c.currentChargeW = 0
		c.waitingForExport = true

		c.dayDischarging = false
		c.dayDischargeSince = time.Time{}
		slog.Info("waiting_for_export", "threshold_w", c.cfg.ExportStartW)
		return c.insight.IdleAllInverters(unitIDs)

	case StateNightDischarge:
		c.currentDischargeW = 300 // start low, ramp up based on grid import

		c.dischargeRampSince = time.Time{}
		c.lastDischargeAdjust = time.Time{}
		return c.writeDischargeRates(c.currentDischargeW)

	case StateIdle, StateStopped, StateSafe:
		return c.insight.IdleAllInverters(unitIDs)

	default:
		return nil
	}
}

// runDayChargeLogic handles daytime charging based on grid export
func (c *Controller) runDayChargeLogic(grid wattnode.GridPower) {
	// Negative grid power = exporting
	// Positive grid power = importing
	gridW := int(grid.Total)
	exportW := 0
	if gridW < 0 {
		exportW = -gridW
	}

	// --- Day peak shave: discharge during expensive day hours ---
	const dayDischargeImportW = 2000
	const dayDischargeHoldDur = 10 * time.Minute
	const dayDischargeMinSOC = 20
	maxDayDischargePerInvW := c.cfg.MaxDischargePerInvW

	soc := c.currentSOC()

	if c.dayDischarging {
		// Currently peak shaving — check exit conditions
		if exportW >= c.cfg.ExportStartW {
			// Solar returned — switch back to charging
			c.stopDayDischarge()
			c.waitingForExport = false
			c.startCharging()
			return
		}
		if soc > 0 && soc <= dayDischargeMinSOC {
			// SOC floor reached — stop discharging, wait for solar
			c.stopDayDischarge()
			return
		}

		// Ramp discharge to keep import in 500-1000W band, same as night logic.
		const dayImportFloor = 500
		const dayImportCeil = 1000
		const dayMinStep = 50
		const dayMaxStep = 300

		if time.Since(c.lastRampAt) >= 60*time.Second {
			if gridW > dayImportCeil && c.currentDischargeW < maxDayDischargePerInvW {
				step := clamp((gridW-dayImportCeil)/4, dayMinStep, dayMaxStep)
				newW := min(c.currentDischargeW+step, maxDayDischargePerInvW)
				slog.Info("day_discharge_ramp_up",
					"import_w", gridW,
					"from_per_inv_w", c.currentDischargeW,
					"to_per_inv_w", newW,
					"step", step,
				)
				c.currentDischargeW = newW
				if err := c.writeDischargeRates(newW); err != nil {
					slog.Error("day discharge ramp failed", "error", err)
				}
				c.lastRampAt = time.Now()
			} else if gridW < dayImportFloor {
				if gridW <= 0 {
					// Exporting — stop peak shave
					c.stopDayDischarge()
					return
				}
				step := clamp((dayImportFloor-gridW)/4, dayMinStep, dayMaxStep)
				newW := max(c.currentDischargeW-step, 0)
				if newW == 0 {
					c.stopDayDischarge()
					return
				}
				slog.Info("day_discharge_ramp_down",
					"import_w", gridW,
					"from_per_inv_w", c.currentDischargeW,
					"to_per_inv_w", newW,
					"step", step,
				)
				c.currentDischargeW = newW
				if err := c.writeDischargeRates(newW); err != nil {
					slog.Error("day discharge ramp failed", "error", err)
				}
				c.lastRampAt = time.Now()
			}
		}
		return // skip normal charge logic while peak shaving
	}

	// Check if we should START peak shaving
	// Only when: not charging (waiting for export or starved), importing > 5kW, SOC > 50%
	if (c.waitingForExport || c.currentChargeW == 0) &&
		gridW > dayDischargeImportW &&
		soc > dayDischargeMinSOC {
		if c.dayDischargeSince.IsZero() {
			c.dayDischargeSince = time.Now()
			slog.Info("day_discharge_timer_started",
				"import_w", gridW,
				"soc", soc,
			)
		} else if time.Since(c.dayDischargeSince) >= dayDischargeHoldDur {
			c.startDayDischarge(gridW, soc)
			return
		}
	} else {
		c.dayDischargeSince = time.Time{}
	}

	// --- Normal charge logic below ---

	// If waiting for export > threshold to start charging
	if c.waitingForExport {
		if exportW >= c.cfg.ExportStartW {
			slog.Info("export_threshold_reached",
				"export_w", exportW,
				"threshold_w", c.cfg.ExportStartW,
			)
			c.waitingForExport = false
			c.startCharging()
		}
		return
	}

	// Dead band check - only act if outside dead band
	if gridW < -c.cfg.DeadBandExportW {
		// Exporting more than dead band - increase charge
		c.rampUpCharge(gridW)
		c.starvationAt = time.Time{} // reset starvation timer
	} else if gridW > c.cfg.DeadBandImportW {
		// Importing more than dead band - decrease charge
		c.rampDownCharge(gridW)
	}

	// Starvation check: at floor with export < 500W for 20 min → idle all
	const starvationExportW = 500
	const starvationTimeout = 20 * time.Minute
	atFloor := c.currentChargeW > 0 && c.currentChargeW <= c.cfg.StartPerInvW
	lowPower := exportW < starvationExportW

	if atFloor && lowPower {
		if c.starvationAt.IsZero() {
			c.starvationAt = time.Now()
			slog.Info("starvation_timer_started",
				"charge_w", c.currentChargeW,
				"export_w", exportW,
			)
		} else if time.Since(c.starvationAt) >= starvationTimeout {
			slog.Info("starvation_idle",
				"duration", time.Since(c.starvationAt),
				"export_w", exportW,
			)
			c.currentChargeW = 0
			c.waitingForExport = true
			c.starvationAt = time.Time{}
			if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
				slog.Error("failed to idle inverters on starvation", "error", err)
			}
		}
	} else {
		c.starvationAt = time.Time{}
	}
}

// startDayDischarge begins peak shave discharge during day
func (c *Controller) startDayDischarge(gridW int, soc int) {
	perInvW := 300 // start low, ramp adjusts every 60s

	c.dayDischarging = true
	c.currentDischargeW = perInvW
	c.currentChargeW = 0
	c.dayDischargeSince = time.Time{}

	if err := c.writeDischargeRates(perInvW); err != nil {
		slog.Error("day discharge start failed", "error", err)
	}
	c.lastRampAt = time.Now()

	slog.Info("day_discharge_start", "import_w", gridW, "per_inv_w", perInvW)
	if c.alerter != nil {
		c.alerter.SendModeChangeAlert("DAY_DISCHARGE",
			fmt.Sprintf("peak shave %dW/inv, SOC %d%%", perInvW, soc))
	}
}

// stopDayDischarge ends peak shave discharge, returns to waiting for export
func (c *Controller) stopDayDischarge() {
	c.dayDischarging = false
	c.currentDischargeW = 0
	c.dayDischargeSince = time.Time{}
	c.waitingForExport = true

	if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
		slog.Error("failed to idle inverters on day discharge stop", "error", err)
	}

	slog.Info("day_discharge_stop")
	if c.alerter != nil {
		c.alerter.SendModeChangeAlert("DAY_CHARGE", "peak shave ended, waiting for export")
	}
}

// chargeExportShare returns the fraction of available energy to use for charging.
// Tapers charge rate as batteries fill to spread export evenly across the day.
//   0-50% SOC:  100% — charge as fast as possible
//   50-75% SOC: 50%  — half to batteries, half to PG&E
//   75-100% SOC: 25% — quarter to batteries, rest to PG&E
// If SOC is unknown (0 = no BMS data yet), assume high and use 25%.
func (c *Controller) chargeExportShare(totalW int) int {
	soc := c.currentSOC()
	if soc == 0 || soc > 75 {
		return totalW / 4
	}
	if soc > 50 {
		return totalW / 2
	}
	return totalW
}

// startCharging begins charging — grabs available export based on SOC
func (c *Controller) startCharging() {
	c.mu.RLock()
	gridW := int(c.gridPower.Total)
	c.mu.RUnlock()

	exportW := 0
	if gridW < 0 {
		exportW = -gridW
	}

	useW := c.chargeExportShare(exportW)
	newTotalW := min(useW, c.cfg.MaxTotalW)
	newChargeW := clamp(newTotalW/4, c.cfg.StartPerInvW, c.cfg.MaxPerInvW)

	c.currentChargeW = newChargeW
	c.stats.RecordChargeRate(c.currentChargeW, true)

	slog.Info("starting_charge",
		"export_w", exportW,
		"per_inv_w", c.currentChargeW,
		"total_w", c.currentChargeW*4,
	)

	if err := c.writeChargeRates(c.currentChargeW); err != nil {
		slog.Error("failed to start charge", "error", err)
		return
	}

	c.lastRampAt = time.Now()
}

// rampUpCharge increases charge rate by taking all available export
func (c *Controller) rampUpCharge(gridW int) {
	// Check hold time (fast ramp up)
	if time.Since(c.lastRampAt) < time.Duration(c.cfg.RampUpHoldSec)*time.Second {
		return
	}

	// Calculate export amount (gridW is negative when exporting)
	exportW := -gridW

	// Total production = what we're already charging + what's still exporting
	currentTotalW := c.currentChargeW * 4
	totalProductionW := currentTotalW + exportW
	// Above 50% SOC, only charge half of total production — rest goes to PG&E
	targetW := c.chargeExportShare(totalProductionW)
	newTotalW := min(targetW, c.cfg.MaxTotalW)
	newChargeW := min(newTotalW/4, c.cfg.MaxPerInvW)

	if newChargeW != c.currentChargeW {
		slog.Info("ramp_up_charge",
			"grid_w", gridW,
			"export_w", exportW,
			"from_per_inv_w", c.currentChargeW,
			"to_per_inv_w", newChargeW,
			"total_w", newChargeW*4,
		)

		if err := c.writeChargeRates(newChargeW); err != nil {
			slog.Error("failed to set charge", "error", err)
			return
		}

		c.currentChargeW = newChargeW
		c.lastRampAt = time.Now()
		c.stats.RecordChargeRate(newChargeW, true)
	}
}

// rampDownCharge decreases charge rate by fixed step, never stops completely
func (c *Controller) rampDownCharge(gridW int) {
	// Check hold time
	if time.Since(c.lastRampAt) < time.Duration(c.cfg.RampDownHoldSec)*time.Second {
		return
	}

	// Trim by half the overshoot — converges in ~3-5 writes, keeps floor
	const trimBufferW = 500
	currentTotalW := c.currentChargeW * 4
	overshoot := gridW - trimBufferW // gridW is positive (importing), keep 500W import buffer
	trimW := max(overshoot/2, 600) // minimum trim step
	newTotalW := max(currentTotalW-trimW, c.cfg.StartPerInvW*4) // floor at starting rate

	newChargeW := newTotalW / 4

	if newChargeW != c.currentChargeW {
		slog.Info("ramp_down_charge",
			"grid_w", gridW,
			"overshoot_w", overshoot,
			"from_per_inv_w", c.currentChargeW,
			"to_per_inv_w", newChargeW,
			"total_w", newChargeW*4,
		)

		if err := c.writeChargeRates(newChargeW); err != nil {
			slog.Error("failed to set charge", "error", err)
			return
		}
		c.currentChargeW = newChargeW

		c.lastRampAt = time.Now()
		c.stats.RecordChargeRate(newChargeW, false)
	}
}

// balancedChargeRates distributes totalW across 4 inverters weighted by SOC deficit.
// If SOC spread is <= 5%, returns equal rates. Otherwise lower SOC gets more.
func (c *Controller) balancedChargeRates(perInvW int) [4]int {
	var rates [4]int

	c.mu.RLock()
	bms := c.bmsStatus
	c.mu.RUnlock()

	// No BMS data or zero rate — equal distribution
	if bms == nil || perInvW <= 0 {
		for i := range rates {
			rates[i] = perInvW
		}
		return rates
	}

	// Find SOC spread
	minSOC, maxSOC := bms.SOC[0], bms.SOC[0]
	for _, s := range bms.SOC[1:] {
		if s < minSOC {
			minSOC = s
		}
		if s > maxSOC {
			maxSOC = s
		}
	}

	const balanceThreshold = 5
	if maxSOC-minSOC <= balanceThreshold {
		for i := range rates {
			rates[i] = perInvW
		}
		return rates
	}

	// Weight inversely by SOC: lower SOC gets more charge
	// deficit = maxSOC - soc + 1 (avoid zero weight)
	totalW := perInvW * 4
	var totalWeight int
	weights := [4]int{}
	for i, s := range bms.SOC {
		weights[i] = maxSOC - s + 1
		totalWeight += weights[i]
	}

	// Distribute proportionally, cap at MaxPerInvW
	var assigned int
	for i := range 4 {
		rates[i] = totalW * weights[i] / totalWeight
		rates[i] = min(rates[i], c.cfg.MaxPerInvW)
		rates[i] = max(rates[i], 0)
		assigned += rates[i]
	}

	// Give remainder to lowest SOC inverter
	if remainder := totalW - assigned; remainder > 0 {
		lowestIdx := 0
		for i, s := range bms.SOC {
			if s < bms.SOC[lowestIdx] {
				lowestIdx = i
			}
		}
		rates[lowestIdx] = min(rates[lowestIdx]+remainder, c.cfg.MaxPerInvW)
	}

	slog.Info("balanced_charge",
		"soc", bms.SOC,
		"spread", maxSOC-minSOC,
		"rates", rates,
	)

	return rates
}

// writeChargeRates writes per-inverter charge rates using SOC balancing.
// Continues on individual inverter errors so one failure doesn't block the rest.
func (c *Controller) writeChargeRates(perInvW int) error {
	rates := c.balancedChargeRates(perInvW)
	unitIDs := c.cfg.AllUnitIDs()
	var firstErr error
	for i, id := range unitIDs {
		if rates[i] > 0 {
			if err := c.insight.SetChargeMode(id, uint16(rates[i])); err != nil {
				slog.Warn("charge write failed", "unit", id, "rate", rates[i], "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		} else {
			if err := c.insight.SetIdleMode(id); err != nil {
				slog.Warn("idle write failed", "unit", id, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}


// writeDischargeRates writes flat discharge rate to all inverters.
// Charge balances SOC, BMS protects low SOC — no balancing needed on discharge.
func (c *Controller) writeDischargeRates(perInvW int) error {
	unitIDs := c.cfg.AllUnitIDs()
	var firstErr error
	for _, id := range unitIDs {
		if perInvW > 0 {
			if err := c.insight.SetDischargeMode(id, uint16(perInvW)); err != nil {
				slog.Warn("discharge write failed", "unit", id, "rate", perInvW, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		} else {
			if err := c.insight.SetIdleMode(id); err != nil {
				slog.Warn("idle write failed", "unit", id, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// writeDischargeKeepalive sends power-only writes (no mode register) for keepalive.
// Flat rate to all inverters.
func (c *Controller) writeDischargeKeepalive(perInvW int) error {
	unitIDs := c.cfg.AllUnitIDs()
	var firstErr error
	for _, id := range unitIDs {
		if err := c.insight.SetDischargePower(id, uint16(perInvW)); err != nil {
			slog.Warn("keepalive discharge write failed", "unit", id, "rate", perInvW, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// writeChargeKeepalive sends power-only writes (no mode register) for keepalive.
// Halves bus time: 4 writes instead of 8 per keepalive cycle.
func (c *Controller) writeChargeKeepalive(perInvW int) error {
	rates := c.balancedChargeRates(perInvW)
	unitIDs := c.cfg.AllUnitIDs()
	var firstErr error
	for i, id := range unitIDs {
		if rates[i] > 0 {
			if err := c.insight.SetChargePower(id, uint16(rates[i])); err != nil {
				slog.Warn("keepalive charge write failed", "unit", id, "rate", rates[i], "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// effectiveExportThreshold returns the net export threshold based on time of day.
// PG&E meters net total across both legs — per-leg monitoring is unnecessary.
func (c *Controller) effectiveExportThreshold() int {
	hour := time.Now().Hour()
	if hour >= 7 && hour < 17 {
		// Solar hours: tolerate 500W export — solar is producing,
		// small net export is normal and not worth reducing discharge for.
		return 500
	}
	return c.cfg.LegExportThresholdW
}

// isSunriseTransition returns true if we're in the hour before charge window
// and solar is starting to produce. Used to skip guard reductions and transition
// directly to DAY_CHARGE.
func (c *Controller) isSunriseTransition() bool {
	hour := time.Now().Hour()
	return hour >= c.scheduler.chargeStartHour-1 && hour < c.scheduler.chargeStartHour
}

// isSunsetGracePeriod returns true during the first 30 minutes of discharge window.
// Solar may still be producing — skip export guard to avoid immediately idling
// all inverters and bouncing back to DAY_CHARGE.
func (c *Controller) isSunsetGracePeriod() bool {
	now := time.Now()
	dischargeStart := time.Date(now.Year(), now.Month(), now.Day(),
		c.scheduler.chargeEndHour, 0, 0, 0, now.Location())
	return now.After(dischargeStart) && now.Before(dischargeStart.Add(30*time.Minute))
}


// runNightDischargeLogic handles nighttime discharge monitoring
func (c *Controller) runNightDischargeLogic(grid wattnode.GridPower) {
	// Skip night guard during manual override in charge window — solar export is expected
	guardActive := !c.manualOverride || !c.scheduler.IsChargeWindow()

	if guardActive {
		// Sunrise: any export → transition to DAY_CHARGE immediately
		if c.isSunriseTransition() && grid.Total < 0 {
			slog.Info("sunrise_transition",
				"l1_w", grid.L1,
				"l2_w", grid.L2,
				"total_w", grid.Total,
			)
			c.currentDischargeW = 0
			if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
				slog.Error("failed to idle on sunrise", "error", err)
			}
			soc := c.currentSOC()
			c.stats.StartSession(SessionCharge, soc)
			c.state.Transition(StateDayCharge, "sunrise — solar export detected")
			c.manualOverride = true
			c.manualOverrideInDay = false
			if err := c.applyCurrentState(); err != nil {
				slog.Error("failed to apply day charge", "error", err)
			}
			return
		}

		// Sunset grace: skip guard for 30 min after discharge window starts
		// Solar still tapering — let small export pass
		if !c.isSunsetGracePeriod() {
			// Export detected — immediate ramp down (no state change, no idling)
			threshold := c.effectiveExportThreshold()
			if grid.Total < float32(-threshold) {
				exportW := int(-grid.Total)
				step := clamp(exportW/4, 50, 300)
				newW := max(c.currentDischargeW-step, 0)
				slog.Warn("night_export_ramp_down",
					"total_w", grid.Total,
					"from_per_inv_w", c.currentDischargeW,
					"to_per_inv_w", newW,
					"step", step,
				)
				c.currentDischargeW = newW
				if newW == 0 {
					if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
						slog.Error("idle all failed", "error", err)
					}
				} else {
					if err := c.writeDischargeRates(newW); err != nil {
						slog.Error("export ramp down failed", "error", err)
					}
				}
				c.lastDischargeAdjust = time.Now()
				return
			}
		}
	}

	// Discharge ramp: adjust every 60s to keep grid import in 500-1000W band.
	// Start low (300W/inv), ramp up if importing too much, ramp down if close to export.
	// Step size scales with headroom: far from target = bigger steps, close = smaller steps.
	const dischargeAdjustInterval = 60 * time.Second
	const importFloor = 500  // below this: ramp down (too close to export)
	const importCeil = 1000  // above this: ramp up (wasting grid power)
	const minStep = 50       // minimum step per adjustment
	const maxStep = 300      // maximum step per adjustment

	if !c.lastDischargeAdjust.IsZero() && time.Since(c.lastDischargeAdjust) < dischargeAdjustInterval {
		return
	}

	importW := int(grid.Total)

	if importW > importCeil && c.currentDischargeW < c.cfg.MaxDischargePerInvW {
		// Importing too much — ramp up discharge
		// Step scales with overshoot: (import - target) / 4 inverters
		headroom := (importW - importCeil) / 4
		step := clamp(headroom, minStep, maxStep)
		newW := min(c.currentDischargeW+step, c.cfg.MaxDischargePerInvW)
		slog.Info("discharge_ramp_up",
			"import_w", importW,
			"from_per_inv_w", c.currentDischargeW,
			"to_per_inv_w", newW,
			"step", step,
			"total_w", newW*4,
		)
		c.currentDischargeW = newW
		if err := c.writeDischargeRates(newW); err != nil {
			slog.Error("discharge ramp write failed", "error", err)
		}
		c.lastDischargeAdjust = time.Now()
	} else if importW < importFloor && c.currentDischargeW > 0 {
		// Too close to export — ramp down discharge
		// Step scales with urgency: closer to 0 = bigger step
		headroom := (importFloor - importW) / 4
		step := clamp(headroom, minStep, maxStep)
		newW := max(c.currentDischargeW-step, 0)
		slog.Info("discharge_ramp_down",
			"import_w", importW,
			"from_per_inv_w", c.currentDischargeW,
			"to_per_inv_w", newW,
			"step", step,
			"total_w", newW*4,
		)
		c.currentDischargeW = newW
		if newW == 0 {
			if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
				slog.Error("idle all failed", "error", err)
			}
		} else {
			if err := c.writeDischargeRates(newW); err != nil {
				slog.Error("discharge ramp write failed", "error", err)
			}
		}
		c.lastDischargeAdjust = time.Now()
	} else {
		// In the 500-1000W sweet spot — do nothing
		if c.lastDischargeAdjust.IsZero() {
			c.lastDischargeAdjust = time.Now()
		}
	}
}


// runNightReducedLogic is now handled by the ramp in runNightDischargeLogic.
// If we end up here, transition back to NIGHT_DISCHARGE.
func (c *Controller) runNightReducedLogic(_ wattnode.GridPower) {
	slog.Info("night_reduced_to_discharge", "reason", "ramp handles export reduction")
	c.state.Transition(StateNightDischarge, "ramp handles discharge adjustment")
}

// enterSafeMode transitions to safe mode
func (c *Controller) enterSafeMode(reason string) {
	if c.state.Current() == StateSafe {
		return
	}

	c.dayDischarging = false
	c.dayDischargeSince = time.Time{}
	c.consecutiveWriteFail = 0

	soc := c.currentSOC()
	c.stats.EndSession(soc)

	slog.Error("entering_safe_mode", "reason", reason)
	c.state.Transition(StateSafe, reason)

	// Idle all inverters immediately
	if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
		slog.Error("failed to idle inverters in safe mode", "error", err)
	}
}

// ManualStop stops all charging/discharging
func (c *Controller) ManualStop() {
	slog.Info("manual_stop_requested")

	c.manualStopped = true
	c.manualOverride = false
	c.dayDischarging = false
	c.dayDischargeSince = time.Time{}

	soc := c.currentSOC()
	c.stats.EndSession(soc)

	c.state.Transition(StateStopped, "manual stop")

	if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
		slog.Error("failed to idle inverters on stop", "error", err)
	}
}

// ManualStart resumes normal operation
func (c *Controller) ManualStart() {
	slog.Info("manual_start_requested")

	c.manualStopped = false
	c.manualOverride = false

	soc := c.currentSOC()

	// Transition back to time-appropriate state
	desiredState := c.scheduler.DesiredState()
	sessionType := SessionDischarge
	if desiredState == StateDayCharge {
		sessionType = SessionCharge
	}
	c.stats.StartSession(sessionType, soc)

	c.state.Transition(desiredState, "manual start")

	if err := c.applyCurrentState(); err != nil {
		slog.Error("failed to apply state on start", "error", err)
	}
}

// ManualCharge forces DAY_CHARGE state regardless of time window
func (c *Controller) ManualCharge() {
	slog.Info("manual_charge_requested")

	c.manualOverride = true
	c.manualOverrideInDay = c.scheduler.IsChargeWindow()
	c.dayDischarging = false
	c.dayDischargeSince = time.Time{}

	soc := c.currentSOC()

	c.stats.StartSession(SessionCharge, soc)
	c.state.Transition(StateDayCharge, "manual charge")

	if err := c.applyCurrentState(); err != nil {
		slog.Error("failed to apply charge state", "error", err)
	}
}

// ManualDischarge forces NIGHT_DISCHARGE state regardless of time window
func (c *Controller) ManualDischarge() {
	slog.Info("manual_discharge_requested")

	c.manualOverride = true
	c.manualOverrideInDay = c.scheduler.IsChargeWindow()
	c.dayDischarging = false
	c.dayDischargeSince = time.Time{}

	soc := c.currentSOC()

	c.stats.StartSession(SessionDischarge, soc)
	c.state.Transition(StateNightDischarge, "manual discharge")

	if err := c.applyCurrentState(); err != nil {
		slog.Error("failed to apply discharge state", "error", err)
	}
}

// setChargeRate sets charge rate on all inverters (for manual /up /down)
func (c *Controller) setChargeRate(perInvW int) {
	slog.Info("manual_charge_rate",
		"from_w", c.currentChargeW,
		"to_w", perInvW,
	)

	if err := c.writeChargeRates(perInvW); err != nil {
		slog.Error("failed to set charge", "error", err)
		return
	}

	c.currentChargeW = perInvW
	c.manualOverride = true
}

// setDischargeRate sets discharge rate on all inverters (for manual /up /down)
func (c *Controller) setDischargeRate(perInvW int) {
	slog.Info("manual_discharge_rate",
		"from_w", c.currentDischargeW,
		"to_w", perInvW,
	)

	if err := c.writeDischargeRates(perInvW); err != nil {
		slog.Error("failed to set discharge", "error", err)
		return
	}

	c.currentDischargeW = perInvW
	c.manualOverride = true
}

// Status returns current controller status
func (c *Controller) Status() ControllerStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return ControllerStatus{
		State:          c.state.Current().String(),
		TimeInState:    c.state.TimeInState(),
		ChargeWindow:   c.scheduler.ChargeWindowString(),
		InChargeWindow: c.scheduler.IsChargeWindow(),
		GridPower:      c.gridPower,
		GridAge:        time.Since(c.lastGridAt),
		BMSStatus:      c.bmsStatus,
		BMSAge:         time.Since(c.lastBMSAt),
		ChargeW:        c.currentChargeW,
		DischargeW:     c.currentDischargeW,
		DayDischarging: c.dayDischarging,
		IdledInverters: c.state.IdledInverters(),
	}
}

// clamp restricts v to the range [lo, hi]
func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// ControllerStatus holds current status for reporting
type ControllerStatus struct {
	State          string
	TimeInState    time.Duration
	ChargeWindow   string
	InChargeWindow bool
	GridPower      wattnode.GridPower
	GridAge        time.Duration
	BMSStatus      *insight.BatteryStatus
	BMSAge         time.Duration
	ChargeW        int
	DischargeW     int
	DayDischarging bool
	IdledInverters []byte
}

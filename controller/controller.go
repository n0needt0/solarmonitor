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
	preGuardDischargeW  int       // Rate before first guard reduction
	dischargeGuardCount int       // Number of guard reduction events (0-4)
	dischargeRampSince  time.Time // When we first saw high import for discharge ramp
	highLoadSince       time.Time // When we first saw high import in NIGHT_REDUCED
	resumeBelowCount    int       // Consecutive readings below resume threshold

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
				keepaliveErr = c.writeDischargeRates(c.currentDischargeW)
				if keepaliveErr != nil {
					slog.Warn("keepalive day discharge failed", "error", keepaliveErr)
				}
			}
		} else if c.currentChargeW > 0 {
			keepaliveErr = c.writeChargeRates(c.currentChargeW)
			if keepaliveErr != nil {
				slog.Warn("keepalive charge failed", "error", keepaliveErr)
			}
		}

	case StateNightDischarge, StateNightReduced:
		if c.currentDischargeW > 0 {
			keepaliveErr = c.writeDischargeRates(c.currentDischargeW)
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
		c.dischargeGuardCount = 0
		c.dayDischarging = false
		c.dayDischargeSince = time.Time{}
		slog.Info("waiting_for_export", "threshold_w", c.cfg.ExportStartW)
		return c.insight.IdleAllInverters(unitIDs)

	case StateNightDischarge:
		c.currentDischargeW = c.touDischargeRate()
		c.dischargeGuardCount = 0
		c.dischargeRampSince = time.Time{}
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

		// Match import: discharge to zero out grid, hold between adjustments
		if time.Since(c.lastRampAt) >= time.Duration(c.cfg.RampUpHoldSec)*time.Second {
			newPerInvW := min(gridW/4, maxDayDischargePerInvW)
			if newPerInvW <= 0 {
				// No longer importing — stop peak shave
				c.stopDayDischarge()
				return
			}
			if newPerInvW != c.currentDischargeW {
				c.currentDischargeW = newPerInvW
				if err := c.writeDischargeRates(newPerInvW); err != nil {
					slog.Error("day discharge adjust failed", "error", err)
				}
				c.lastRampAt = time.Now()
				slog.Info("day_discharge_adjust",
					"import_w", gridW,
					"per_inv_w", newPerInvW,
				)
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
	perInvW := clamp(gridW/4, 100, c.cfg.MaxDischargePerInvW)

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

// startCharging begins charging — grabs all available export immediately
func (c *Controller) startCharging() {
	c.mu.RLock()
	gridW := int(c.gridPower.Total)
	c.mu.RUnlock()

	// Take all available export, minimum StartPerInvW
	exportW := 0
	if gridW < 0 {
		exportW = -gridW
	}

	newTotalW := min(exportW, c.cfg.MaxTotalW)
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

	// Take all available export: new_total = current_total + export
	currentTotalW := c.currentChargeW * 4
	newTotalW := min(currentTotalW+exportW, c.cfg.MaxTotalW)
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

// balancedDischargeRates distributes totalW across 4 inverters weighted by SOC excess.
// Master (index 0) is protected: its effective SOC is raised by 10% so it always
// gets less discharge, preserving headroom above its 25% floor vs 15% for slaves.
// If SOC spread is <= 5% (after adjustment), returns equal rates.
func (c *Controller) balancedDischargeRates(perInvW int) [4]int {
	var rates [4]int

	c.mu.RLock()
	bms := c.bmsStatus
	c.mu.RUnlock()

	if bms == nil || perInvW <= 0 {
		for i := range rates {
			rates[i] = perInvW
		}
		return rates
	}

	// Adjusted SOC: master gets -10% so it appears lower, receiving less discharge.
	// This keeps master SOC ~10% above slaves, matching the 25% vs 15% floor gap.
	const masterProtection = 10
	adjSOC := bms.SOC
	adjSOC[0] -= masterProtection // index 0 = master appears lower → less discharge

	minSOC, maxSOC := adjSOC[0], adjSOC[0]
	for _, s := range adjSOC[1:] {
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

	// Weight by adjusted SOC excess: higher adjusted SOC gets more discharge
	// excess = adjSOC - minSOC + 1 (avoid zero weight)
	totalW := perInvW * 4
	var totalWeight int
	weights := [4]int{}
	for i, s := range adjSOC {
		weights[i] = s - minSOC + 1
		totalWeight += weights[i]
	}

	maxDischPerInv := c.cfg.MaxDischargePerInvW
	var assigned int
	for i := range 4 {
		rates[i] = totalW * weights[i] / totalWeight
		rates[i] = min(rates[i], maxDischPerInv)
		rates[i] = max(rates[i], 0)
		assigned += rates[i]
	}

	// Give remainder to highest adjusted SOC inverter (never master if tied)
	if remainder := totalW - assigned; remainder > 0 {
		highestIdx := 1 // start from first slave
		for i := 1; i < 4; i++ {
			if adjSOC[i] > adjSOC[highestIdx] {
				highestIdx = i
			}
		}
		rates[highestIdx] = min(rates[highestIdx]+remainder, maxDischPerInv)
	}

	slog.Info("balanced_discharge",
		"soc", bms.SOC,
		"adj_soc", adjSOC,
		"spread", maxSOC-minSOC,
		"rates", rates,
	)

	return rates
}

// writeDischargeRates writes per-inverter discharge rates using SOC balancing.
// Continues on individual inverter errors so one failure doesn't block the rest.
func (c *Controller) writeDischargeRates(perInvW int) error {
	rates := c.balancedDischargeRates(perInvW)
	unitIDs := c.cfg.AllUnitIDs()
	var firstErr error
	for i, id := range unitIDs {
		if rates[i] > 0 {
			if err := c.insight.SetDischargeMode(id, uint16(rates[i])); err != nil {
				slog.Warn("discharge write failed", "unit", id, "rate", rates[i], "error", err)
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

// effectiveExportThreshold returns the per-leg export threshold based on time of day.
// During solar hours (8am-5pm), tolerate 100W/leg from residual production.
// Outside solar hours, use the configured threshold (50W noise tolerance).
func (c *Controller) effectiveExportThreshold() int {
	hour := time.Now().Hour()
	if hour >= 8 && hour < 17 {
		return 100
	}
	return c.cfg.LegExportThresholdW
}

// touDischargeRate returns the per-inverter discharge rate based on TOU peak window.
// Peak (5pm-9pm): 1200W/inv (4.8kW total) — displaces expensive $0.50/kWh grid power.
// Off-peak: base rate from config (600W/inv).
func (c *Controller) touDischargeRate() int {
	hour := time.Now().Hour()
	if hour >= 17 && hour < 21 {
		return c.cfg.DischargePerInvW * 2 // 1200W during peak
	}
	return c.cfg.DischargePerInvW // 600W off-peak
}

// runNightDischargeLogic handles nighttime discharge monitoring
func (c *Controller) runNightDischargeLogic(grid wattnode.GridPower) {
	// Skip night guard during manual override in charge window — solar export is expected
	if !c.manualOverride || !c.scheduler.IsChargeWindow() {
		// Night guard: check each leg against effective threshold
		threshold := c.effectiveExportThreshold()
		if grid.L1 < float32(-threshold) ||
			grid.L2 < float32(-threshold) {
			c.nightExportDetected(grid)
			c.dischargeRampSince = time.Time{}
			return
		}
	}

	// TOU peak/off-peak rate transition (only if guard hasn't reduced us and no manual override)
	if c.dischargeGuardCount == 0 && !c.manualOverride {
		touRate := c.touDischargeRate()
		if touRate != c.currentDischargeW {
			slog.Info("tou_rate_change",
				"from_per_inv_w", c.currentDischargeW,
				"to_per_inv_w", touRate,
				"peak", time.Now().Hour() >= 17 && time.Now().Hour() < 21,
			)
			c.currentDischargeW = touRate
			if err := c.writeDischargeRates(touRate); err != nil {
				slog.Error("tou rate change failed", "error", err)
			}
		}
	}

	// Dynamic discharge ramp: if importing > 2kW for 5 min, increase by 100W/inv
	const dischargeRampImportW = 2000
	const dischargeRampHoldDur = 5 * time.Minute
	const dischargeRampStepW = 100

	if c.currentDischargeW >= c.cfg.MaxDischargePerInvW {
		return // already at max
	}

	if grid.Total > float32(dischargeRampImportW) {
		if c.dischargeRampSince.IsZero() {
			c.dischargeRampSince = time.Now()
			slog.Info("discharge_ramp_timer_started",
				"import_w", grid.Total,
				"current_per_inv_w", c.currentDischargeW,
			)
		} else if time.Since(c.dischargeRampSince) >= dischargeRampHoldDur {
			newW := min(c.currentDischargeW+dischargeRampStepW, c.cfg.MaxDischargePerInvW)
			slog.Info("discharge_ramp_up",
				"import_w", grid.Total,
				"from_per_inv_w", c.currentDischargeW,
				"to_per_inv_w", newW,
				"total_w", newW*4,
			)
			c.currentDischargeW = newW
			if err := c.writeDischargeRates(newW); err != nil {
				slog.Error("discharge ramp write failed", "error", err)
			}
			c.dischargeRampSince = time.Now() // reset for next step
		}
	} else {
		c.dischargeRampSince = time.Time{}
	}
}

// nightExportDetected handles export detection at night
// Fast response: idle one inverter immediately, then rebalance all 4 at reduced rate
func (c *Controller) nightExportDetected(grid wattnode.GridPower) {
	slog.Warn("night_export_detected",
		"l1_w", grid.L1,
		"l2_w", grid.L2,
		"total_w", grid.Total,
	)

	if c.dischargeGuardCount >= 4 {
		return // already fully reduced
	}

	// Snapshot rate before first reduction
	if c.dischargeGuardCount == 0 {
		c.preGuardDischargeW = c.currentDischargeW
	}

	c.dischargeGuardCount++
	activeEquiv := 4 - c.dischargeGuardCount

	if activeEquiv <= 0 {
		// Fully reduced — idle all
		c.currentDischargeW = 0
		for _, id := range c.cfg.AllUnitIDs() {
			if err := c.insight.SetIdleMode(id); err != nil {
				slog.Error("failed to idle inverter", "unit", id, "error", err)
			}
		}

		c.stats.RecordNightGuardEvent(0, grid.L1, grid.L2, activeEquiv)

		// Solar is clearly producing — transition to DAY_CHARGE immediately
		soc := c.currentSOC()

		slog.Info("night_guard_early_charge",
			"guard_count", c.dischargeGuardCount,
			"l1_w", grid.L1,
			"l2_w", grid.L2,
		)
		c.stats.StartSession(SessionCharge, soc)
		c.state.Transition(StateDayCharge, "solar export idled all inverters — starting charge")

		// Set manual override so checkTimeTransitions doesn't bounce us
		// back to NIGHT_DISCHARGE before the charge window opens
		c.manualOverride = true
		c.manualOverrideInDay = false // override was set during discharge window

		if err := c.applyCurrentState(); err != nil {
			slog.Error("failed to apply day charge state", "error", err)
		}
		return
	}

	// Step 1: idle one inverter immediately for fast export reduction
	idleTarget := c.cfg.IdleOrder[c.dischargeGuardCount-1]
	if err := c.insight.SetIdleMode(idleTarget); err != nil {
		slog.Error("failed to idle inverter", "unit", idleTarget, "error", err)
	}

	// Step 2: rebalance — spread reduced total across all 4 with SOC balancing
	c.currentDischargeW = c.preGuardDischargeW * activeEquiv / 4
	slog.Info("night_guard_rebalance",
		"guard_count", c.dischargeGuardCount,
		"idled_fast", idleTarget,
		"per_inv_w", c.currentDischargeW,
		"total_w", c.currentDischargeW*4,
	)
	if err := c.writeDischargeRates(c.currentDischargeW); err != nil {
		slog.Error("failed to rebalance discharge", "error", err)
	}

	c.stats.RecordNightGuardEvent(0, grid.L1, grid.L2, activeEquiv)

	if c.state.Current() != StateNightReduced {
		c.state.Transition(StateNightReduced, fmt.Sprintf("leg export detected, reduced to %dW/inv", c.currentDischargeW))
	}
}

// runNightReducedLogic monitors for continued export and reduces further if needed
func (c *Controller) runNightReducedLogic(grid wattnode.GridPower) {
	// Skip night guard during manual override in charge window — solar export is expected
	if !c.manualOverride || !c.scheduler.IsChargeWindow() {
		// Still monitoring for export — if export returns, reduce further
		threshold := c.effectiveExportThreshold()
		if grid.L1 < float32(-threshold) ||
			grid.L2 < float32(-threshold) {
			c.nightExportDetected(grid)
			c.highLoadSince = time.Time{}
			c.resumeBelowCount = 0
			return
		}
	}

	// Resume logic: if importing > 2× the increase from undoing one guard step,
	// sustained for 5 min, undo one guard reduction.
	// Each step adds preGuardDischargeW total watts; require 2× that as headroom.
	resumeImportW := c.preGuardDischargeW * 2
	if resumeImportW < 600 {
		resumeImportW = 600 // floor: at least 600W import to resume
	}
	const resumeHoldDur = 5 * time.Minute

	if c.dischargeGuardCount <= 0 {
		// Fully resumed — transition back to NIGHT_DISCHARGE
		c.state.Transition(StateNightDischarge, "discharge fully restored")
		c.highLoadSince = time.Time{}
		c.resumeBelowCount = 0
		return
	}

	if grid.Total > float32(resumeImportW) {
		c.resumeBelowCount = 0
		if c.highLoadSince.IsZero() {
			c.highLoadSince = time.Now()
			slog.Info("night_resume_timer_started",
				"import_w", grid.Total,
				"guard_count", c.dischargeGuardCount,
			)
		} else if time.Since(c.highLoadSince) >= resumeHoldDur {
			// Undo one guard reduction — increase rate on all 4
			c.dischargeGuardCount--
			if c.dischargeGuardCount <= 0 {
				c.currentDischargeW = c.preGuardDischargeW
			} else {
				c.currentDischargeW = c.preGuardDischargeW * (4 - c.dischargeGuardCount) / 4
			}

			slog.Info("night_resume_increase",
				"import_w", grid.Total,
				"guard_count", c.dischargeGuardCount,
				"per_inv_w", c.currentDischargeW,
				"total_w", c.currentDischargeW*4,
			)

			if err := c.writeDischargeRates(c.currentDischargeW); err != nil {
				slog.Error("failed to resume discharge", "error", err)
			}

			c.highLoadSince = time.Time{} // reset for next step
		}
	} else {
		// Load dropped — require 3 consecutive readings below threshold to reset timer
		if !c.highLoadSince.IsZero() {
			c.resumeBelowCount++
			if c.resumeBelowCount >= 3 {
				c.highLoadSince = time.Time{}
				c.resumeBelowCount = 0
			}
		}
	}
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

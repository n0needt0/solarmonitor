package controller

import (
	"context"
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
	DischargePerInvW int

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

// Stats returns the session stats tracker
func (c *Controller) Stats() *StatsTracker {
	return c.stats
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

	// Only send keepalives in active states
	if state == StateStopped || state == StateSafe || state == StateIdle {
		return
	}

	slog.Debug("sending_keepalive", "state", state.String())

	switch state {
	case StateDayCharge:
		// Re-send current charge level
		if c.currentChargeW > 0 {
			for _, id := range c.cfg.AllUnitIDs() {
				if err := c.insight.SetChargeMode(id, uint16(c.currentChargeW)); err != nil {
					slog.Warn("keepalive charge failed", "unit", id, "error", err)
				}
			}
		}
		c.lastKeepaliveAt = time.Now()

	case StateNightDischarge:
		// Re-send discharge mode (EPC=2) and limit to active inverters
		idled := c.state.IdledInverters()
		for _, id := range c.cfg.AllUnitIDs() {
			isIdled := false
			for _, idledID := range idled {
				if id == idledID {
					isIdled = true
					break
				}
			}
			if !isIdled {
				if err := c.insight.SetDischargeMode(id, uint16(c.cfg.DischargePerInvW)); err != nil {
					slog.Warn("keepalive discharge failed", "unit", id, "error", err)
				}
			}
		}
		c.lastKeepaliveAt = time.Now()

	case StateNightReduced:
		// Re-send current state - some discharging, some idled
		idled := c.state.IdledInverters()
		for _, id := range c.cfg.AllUnitIDs() {
			isIdled := false
			for _, idledID := range idled {
				if id == idledID {
					isIdled = true
					break
				}
			}
			if isIdled {
				if err := c.insight.SetIdleMode(id); err != nil {
					slog.Warn("keepalive idle failed", "unit", id, "error", err)
				}
			} else {
				if err := c.insight.SetDischargeMode(id, uint16(c.cfg.DischargePerInvW)); err != nil {
					slog.Warn("keepalive discharge failed", "unit", id, "error", err)
				}
			}
		}
		c.lastKeepaliveAt = time.Now()
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

	// Don't override STOPPED or SAFE states
	if currentState == StateStopped || currentState == StateSafe {
		return
	}

	// Get current SOC for stats
	soc := 0
	c.mu.RLock()
	if c.bmsStatus != nil {
		soc = c.bmsStatus.TotalSOC()
	}
	c.mu.RUnlock()

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
			c.stats.StartSession(SessionDischarge, soc)
			c.state.Transition(StateNightDischarge, "entering discharge window")
			if err := c.applyCurrentState(); err != nil {
				slog.Error("failed to apply night discharge state", "error", err)
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
	case StateIdle:
		// All inverters idle
		return c.insight.IdleAllInverters(unitIDs)

	case StateDayCharge:
		// Don't start charging immediately - wait for export > threshold
		c.currentChargeW = 0
		c.waitingForExport = true
		slog.Info("waiting_for_export", "threshold_w", c.cfg.ExportStartW)
		// Inverters stay idle until we see enough export
		return c.insight.IdleAllInverters(unitIDs)

	case StateNightDischarge:
		// Set EPC mode to discharge (2) and set discharge limit on all inverters
		for _, id := range unitIDs {
			if err := c.insight.SetDischargeMode(id, uint16(c.cfg.DischargePerInvW)); err != nil {
				return err
			}
		}
		return nil

	case StateNightReduced:
		// Already handled by night guard logic
		return nil

	case StateStopped:
		return c.insight.IdleAllInverters(unitIDs)

	case StateSafe:
		// Emergency idle all
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
	} else if !atFloor || !lowPower {
		if !c.starvationAt.IsZero() {
			c.starvationAt = time.Time{}
		}
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

	newTotalW := exportW
	if newTotalW > c.cfg.MaxTotalW {
		newTotalW = c.cfg.MaxTotalW
	}

	newChargeW := newTotalW / 4
	if newChargeW < c.cfg.StartPerInvW {
		newChargeW = c.cfg.StartPerInvW
	}
	if newChargeW > c.cfg.MaxPerInvW {
		newChargeW = c.cfg.MaxPerInvW
	}

	c.currentChargeW = newChargeW
	c.stats.RecordChargeRate(c.currentChargeW, true)

	slog.Info("starting_charge",
		"export_w", exportW,
		"per_inv_w", c.currentChargeW,
		"total_w", c.currentChargeW*4,
	)

	for _, id := range c.cfg.AllUnitIDs() {
		if err := c.insight.SetChargeMode(id, uint16(c.currentChargeW)); err != nil {
			slog.Error("failed to start charge", "unit", id, "error", err)
			return
		}
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
	newTotalW := currentTotalW + exportW
	if newTotalW > c.cfg.MaxTotalW {
		newTotalW = c.cfg.MaxTotalW
	}

	newChargeW := newTotalW / 4
	if newChargeW > c.cfg.MaxPerInvW {
		newChargeW = c.cfg.MaxPerInvW
	}

	if newChargeW != c.currentChargeW {
		slog.Info("ramp_up_charge",
			"grid_w", gridW,
			"export_w", exportW,
			"from_per_inv_w", c.currentChargeW,
			"to_per_inv_w", newChargeW,
			"total_w", newChargeW*4,
		)

		for _, id := range c.cfg.AllUnitIDs() {
			if err := c.insight.SetChargeMode(id, uint16(newChargeW)); err != nil {
				slog.Error("failed to set charge", "unit", id, "error", err)
				return
			}
		}

		c.currentChargeW = newChargeW
		c.lastRampAt = time.Now()
		c.stats.RecordChargeRate(newChargeW, true)
	}
}

// rampDownCharge decreases charge rate by fixed step, never stops completely
func (c *Controller) rampDownCharge(gridW int) {
	// Check hold time (slow ramp down — ride through clouds)
	if time.Since(c.lastRampAt) < time.Duration(c.cfg.RampDownHoldSec)*time.Second {
		return
	}

	// Drop by fixed step (600W total = 150W/inv)
	const trimStepW = 600
	currentTotalW := c.currentChargeW * 4
	newTotalW := currentTotalW - trimStepW

	// Never go below starting rate — keep charging
	minTotalW := c.cfg.StartPerInvW * 4
	if newTotalW < minTotalW {
		newTotalW = minTotalW
	}

	newChargeW := newTotalW / 4

	if newChargeW != c.currentChargeW {
		slog.Info("ramp_down_charge",
			"grid_w", gridW,
			"from_per_inv_w", c.currentChargeW,
			"to_per_inv_w", newChargeW,
			"total_w", newChargeW*4,
		)

		for _, id := range c.cfg.AllUnitIDs() {
			if err := c.insight.SetChargeMode(id, uint16(newChargeW)); err != nil {
				slog.Error("failed to set charge", "unit", id, "error", err)
				return
			}
		}

		c.currentChargeW = newChargeW
		c.lastRampAt = time.Now()
		c.stats.RecordChargeRate(newChargeW, false)
	}
}

// runNightDischargeLogic handles nighttime discharge monitoring
func (c *Controller) runNightDischargeLogic(grid wattnode.GridPower) {
	// Night guard: zero tolerance for export
	// Check each leg individually
	if grid.L1 < float32(-c.cfg.LegExportThresholdW) ||
		grid.L2 < float32(-c.cfg.LegExportThresholdW) {
		c.nightExportDetected(grid)
	}
}

// nightExportDetected handles export detection at night — idle inverters until no export
func (c *Controller) nightExportDetected(grid wattnode.GridPower) {
	slog.Warn("night_export_detected",
		"l1_w", grid.L1,
		"l2_w", grid.L2,
		"total_w", grid.Total,
	)

	idledCount := len(c.state.IdledInverters())
	if idledCount >= 4 {
		// All already idled, nothing more to do
		return
	}

	// Get next inverter to idle from idle_order
	nextToIdle := c.cfg.IdleOrder[idledCount]

	slog.Info("idling_inverter_for_export",
		"unit", nextToIdle,
		"idled_count", idledCount+1,
	)

	if err := c.insight.SetIdleMode(nextToIdle); err != nil {
		slog.Error("failed to idle inverter", "unit", nextToIdle, "error", err)
		return
	}

	c.state.AddIdledInverter(nextToIdle)
	activeAfter := 4 - (idledCount + 1)
	c.stats.RecordNightGuardEvent(nextToIdle, grid.L1, grid.L2, activeAfter)

	// Transition to NIGHT_REDUCED after first idle
	if c.state.Current() != StateNightReduced {
		c.state.Transition(StateNightReduced, "leg export detected, idled inverter(s)")
	}
}

// runNightReducedLogic monitors for continued export and idles more inverters
func (c *Controller) runNightReducedLogic(grid wattnode.GridPower) {
	// Still monitoring for export — if export returns, idle the next inverter
	if grid.L1 < float32(-c.cfg.LegExportThresholdW) ||
		grid.L2 < float32(-c.cfg.LegExportThresholdW) {
		c.nightExportDetected(grid)
	}
}

// enterSafeMode transitions to safe mode
func (c *Controller) enterSafeMode(reason string) {
	if c.state.Current() == StateSafe {
		return
	}

	soc := 0
	c.mu.RLock()
	if c.bmsStatus != nil {
		soc = c.bmsStatus.TotalSOC()
	}
	c.mu.RUnlock()
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

	soc := 0
	c.mu.RLock()
	if c.bmsStatus != nil {
		soc = c.bmsStatus.TotalSOC()
	}
	c.mu.RUnlock()
	c.stats.EndSession(soc)

	c.state.Transition(StateStopped, "manual stop")

	if err := c.insight.IdleAllInverters(c.cfg.AllUnitIDs()); err != nil {
		slog.Error("failed to idle inverters on stop", "error", err)
	}
}

// ManualStart resumes normal operation
func (c *Controller) ManualStart() {
	slog.Info("manual_start_requested")

	soc := 0
	c.mu.RLock()
	if c.bmsStatus != nil {
		soc = c.bmsStatus.TotalSOC()
	}
	c.mu.RUnlock()

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

// setChargeRate sets charge rate on all inverters (for manual /up /down)
func (c *Controller) setChargeRate(perInvW int) {
	slog.Info("manual_charge_rate",
		"from_w", c.currentChargeW,
		"to_w", perInvW,
	)

	for _, id := range c.cfg.AllUnitIDs() {
		if perInvW > 0 {
			if err := c.insight.SetChargeMode(id, uint16(perInvW)); err != nil {
				slog.Error("failed to set charge", "unit", id, "error", err)
				return
			}
		} else {
			if err := c.insight.SetIdleMode(id); err != nil {
				slog.Error("failed to idle", "unit", id, "error", err)
				return
			}
		}
	}

	c.currentChargeW = perInvW
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
		IdledInverters: c.state.IdledInverters(),
	}
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
	IdledInverters []byte
}

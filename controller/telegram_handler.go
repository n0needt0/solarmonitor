package controller

import (
	"time"

	"github.com/n0needt0/solarcontrol/telegram"
)

// TelegramHandler implements telegram.CommandHandler
type TelegramHandler struct {
	ctrl      *Controller
	bot       *telegram.Bot
	startTime time.Time
	stepW     int // Manual adjustment step size
}

// NewTelegramHandler creates a handler for telegram commands
func NewTelegramHandler(ctrl *Controller, bot *telegram.Bot, stepW int) *TelegramHandler {
	return &TelegramHandler{
		ctrl:      ctrl,
		bot:       bot,
		startTime: time.Now(),
		stepW:     stepW,
	}
}

// HandleStatus returns current status
func (h *TelegramHandler) HandleStatus() string {
	status := h.ctrl.Status()

	currentState := h.ctrl.state.Current()
	unitIDs := h.ctrl.cfg.AllUnitIDs()

	var inverters [4]telegram.InverterData
	for i, uid := range unitIDs {
		inv := telegram.InverterData{UnitID: uid}

		if status.BMSStatus != nil {
			inv.SOC = status.BMSStatus.SOC[i]
			inv.ActualW = int(status.BMSStatus.Power[i])
		}

		switch currentState {
		case StateDayCharge:
			if status.DayDischarging {
				inv.TargetW = -status.DischargeW
			} else {
				inv.TargetW = status.ChargeW
			}
		case StateNightDischarge, StateNightReduced:
			inv.TargetW = status.DischargeW
		}

		inverters[i] = inv
	}

	data := telegram.StatusData{
		State:          status.State,
		TimeInState:    status.TimeInState,
		GridL1:         status.GridPower.L1,
		GridL2:         status.GridPower.L2,
		GridTotal:      status.GridPower.Total,
		Inverters:      inverters,
		ChargeW:        status.ChargeW,
		DayDischarging: status.DayDischarging,
		IdledInverters: status.IdledInverters,
		Uptime:         time.Since(h.startTime),
	}

	return telegram.FormatStatus(data)
}

// HandleStop stops all inverters
func (h *TelegramHandler) HandleStop() string {
	h.ctrl.ManualStop()

	status := h.ctrl.Status()
	var avgSOC int
	if status.BMSStatus != nil {
		avgSOC = status.BMSStatus.TotalSOC()
	}

	return telegram.FormatStopped(status.GridPower.L1, status.GridPower.L2, avgSOC)
}

// HandleStart resumes operation
func (h *TelegramHandler) HandleStart() string {
	h.ctrl.ManualStart()

	status := h.ctrl.Status()
	var avgSOC int
	if status.BMSStatus != nil {
		avgSOC = status.BMSStatus.TotalSOC()
	}

	return telegram.FormatStarted(status.State, status.GridPower.L1, status.GridPower.L2, avgSOC)
}

// HandleUp increases charge/discharge rate
func (h *TelegramHandler) HandleUp() string {
	state := h.ctrl.state.Current()

	switch state {
	case StateDayCharge:
		// Increase charge rate
		newW := h.ctrl.currentChargeW + h.stepW
		if newW > h.ctrl.cfg.MaxPerInvW {
			return "Already at maximum charge rate"
		}
		h.ctrl.setChargeRate(newW)
		return h.HandleStatus()

	case StateNightDischarge:
		newW := h.ctrl.currentDischargeW + h.stepW
		if newW > 1200 {
			return "Already at maximum discharge rate (1200W/inv)"
		}
		h.ctrl.setDischargeRate(newW)
		return h.HandleStatus()

	case StateNightReduced:
		return "Cannot increase in NIGHT_REDUCED"

	case StateStopped:
		return "Controller is stopped. Use /start first"

	default:
		return "Cannot adjust in current state"
	}
}

// HandleDown decreases charge/discharge rate
func (h *TelegramHandler) HandleDown() string {
	state := h.ctrl.state.Current()

	switch state {
	case StateDayCharge:
		// Decrease charge rate
		newW := h.ctrl.currentChargeW - h.stepW
		if newW < 0 {
			newW = 0
		}
		h.ctrl.setChargeRate(newW)
		return h.HandleStatus()

	case StateNightDischarge, StateNightReduced:
		newW := h.ctrl.currentDischargeW - h.stepW
		if newW < 0 {
			newW = 0
		}
		h.ctrl.setDischargeRate(newW)
		return h.HandleStatus()

	case StateStopped:
		return "Controller is stopped. Use /start first"

	default:
		return "Cannot adjust in current state"
	}
}

// HandleCharge forces charge mode regardless of time window
func (h *TelegramHandler) HandleCharge() string {
	h.ctrl.ManualCharge()
	return h.HandleStatus()
}

// HandleDischarge forces discharge mode regardless of time window
func (h *TelegramHandler) HandleDischarge() string {
	h.ctrl.ManualDischarge()
	return h.HandleStatus()
}

// HandleStats returns session statistics
func (h *TelegramHandler) HandleStats() string {
	var current, lastCharge, lastDischarge *telegram.SessionStatsData

	if s := h.ctrl.Stats().CurrentSession(); s != nil {
		d := toStatsData(s)
		// For in-progress session, use current SOC as EndSOC
		status := h.ctrl.Status()
		if status.BMSStatus != nil {
			d.EndSOC = status.BMSStatus.TotalSOC()
		}
		current = &d
	}
	if s := h.ctrl.Stats().LastCharge(); s != nil {
		d := toStatsData(s)
		lastCharge = &d
	}
	if s := h.ctrl.Stats().LastDischarge(); s != nil {
		d := toStatsData(s)
		lastDischarge = &d
	}

	return telegram.FormatStats(current, lastCharge, lastDischarge)
}

func toStatsData(s *SessionStats) telegram.SessionStatsData {
	d := telegram.SessionStatsData{
		Type:                 s.Type.String(),
		StartTime:            s.StartTime,
		EndTime:              s.EndTime,
		StartSOC:             s.StartSOC,
		EndSOC:               s.EndSOC,
		TotalEnergyWh:        s.TotalEnergyWh,
		GridEnergyWh:         s.GridEnergyWh,
		AvgGridPowerW:        s.AvgGridPowerW(),
		PeakChargePerInvW:    s.PeakChargePerInvW,
		PeakChargeTotalW:     s.PeakChargeTotalW,
		RampUpCount:          s.RampUpCount,
		RampDownCount:        s.RampDownCount,
		TimeAtMaxSec:         s.TimeAtMaxSec,
		MaxRollingExport5min: s.MaxRollingExport5min,
		MaxRollingImport5min: s.MaxRollingImport5min,
		MaxRollingExport1min: s.MaxRollingExport1min,
	}
	for _, ev := range s.NightGuardEvents {
		d.NightGuardEvents = append(d.NightGuardEvents, telegram.NightGuardEventData{
			Time:        ev.Time,
			UnitID:      ev.UnitID,
			L1Before:    ev.L1Before,
			L2Before:    ev.L2Before,
			ActiveAfter: ev.ActiveAfter,
		})
	}
	return d
}

// SendModeChangeAlert sends mode change notification
func (h *TelegramHandler) SendModeChangeAlert(state string, detail string) {
	status := h.ctrl.Status()
	var avgSOC int
	if status.BMSStatus != nil {
		avgSOC = status.BMSStatus.TotalSOC()
	}

	msg := telegram.FormatModeChange(state, status.GridPower.L1, status.GridPower.L2, avgSOC, detail)
	h.bot.Alert(telegram.AlertModeChange, msg)
}

// SendNightReducedAlert sends night reduced notification
func (h *TelegramHandler) SendNightReducedAlert(l1Before, l2Before, l1After, l2After float32, activeCount int, idled []byte) {
	var avgSOC int
	if h.ctrl.bmsStatus != nil {
		avgSOC = h.ctrl.bmsStatus.TotalSOC()
	}

	msg := telegram.FormatNightReduced(l1Before, l2Before, l1After, l2After, avgSOC, activeCount, idled)
	h.bot.AlertImmediate(msg) // Always send night reduced alerts
}

// SendFailureAlert sends failure notification
func (h *TelegramHandler) SendFailureAlert(reason string) {
	status := h.ctrl.Status()
	var avgSOC int
	if status.BMSStatus != nil {
		avgSOC = status.BMSStatus.TotalSOC()
	}

	msg := telegram.FormatFailure(reason, status.GridPower.L1, status.GridPower.L2, avgSOC)
	h.bot.Alert(telegram.AlertFailure, msg)
}

// SendRecoveryAlert sends recovery notification
func (h *TelegramHandler) SendRecoveryAlert(state string) {
	status := h.ctrl.Status()
	var avgSOC int
	if status.BMSStatus != nil {
		avgSOC = status.BMSStatus.TotalSOC()
	}

	msg := telegram.FormatRecovery(state, status.GridPower.L1, status.GridPower.L2, avgSOC)
	h.bot.Alert(telegram.AlertRecovery, msg)
}

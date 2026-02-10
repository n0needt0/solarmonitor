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

	var soc [4]int
	avgSOC := 0
	if status.BMSStatus != nil {
		soc = status.BMSStatus.SOC
		avgSOC = status.BMSStatus.TotalSOC()
	}

	dischargeW := 0
	if h.ctrl.state.Current() == StateNightDischarge || h.ctrl.state.Current() == StateNightReduced {
		dischargeW = h.ctrl.cfg.DischargePerInvW
	}

	data := telegram.StatusData{
		State:          status.State,
		TimeInState:    status.TimeInState,
		GridL1:         status.GridPower.L1,
		GridL2:         status.GridPower.L2,
		GridTotal:      status.GridPower.Total,
		SOC:            soc,
		AvgSOC:         avgSOC,
		ChargeW:        status.ChargeW,
		DischargeW:     dischargeW,
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

	case StateNightReduced:
		// Cannot increase in reduced mode
		return "Cannot increase in NIGHT_REDUCED - only down allowed"

	case StateNightDischarge:
		// Could increase discharge but not implemented
		return "Discharge rate is fixed at 600W/inverter"

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
		// Manual idle of next inverter
		return "Use night guard for discharge reduction"

	case StateStopped:
		return "Controller is stopped. Use /start first"

	default:
		return "Cannot adjust in current state"
	}
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

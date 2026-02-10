package telegram

import (
	"fmt"
	"time"
)

// AlertType constants for rate limiting
const (
	AlertModeChange    = "mode_change"
	AlertNightReduced  = "night_reduced"
	AlertBatteriesFull = "batteries_full"
	AlertFailure       = "failure"
	AlertRecovery      = "recovery"
)

// InverterData holds per-inverter status
type InverterData struct {
	UnitID   byte
	SOC      int
	ActualW  int // Actual battery power (positive=charging, negative=discharging)
	TargetW  int // Commanded EPC power
	Idled    bool
}

// StatusData holds data for status response
type StatusData struct {
	State          string
	TimeInState    time.Duration
	GridL1         float32
	GridL2         float32
	GridTotal      float32
	Inverters      [4]InverterData
	AvgSOC         int
	ChargeW        int
	DischargeW     int
	IdledInverters []byte
	Uptime         time.Duration
}

// FormatStatus formats status data for Telegram
func FormatStatus(d StatusData) string {
	gridStatus := "importing"
	gridAmount := d.GridTotal
	if d.GridTotal < 0 {
		gridStatus = "exporting"
		gridAmount = -d.GridTotal
	}

	deadBand := ""
	if d.GridTotal >= -1000 && d.GridTotal <= 2000 {
		deadBand = " (dead band)"
	}

	msg := fmt.Sprintf("State:  %s (%s)\n", d.State, formatDuration(d.TimeInState))
	msg += fmt.Sprintf("Grid:   %s %.1fkW%s\n", gridStatus, gridAmount/1000, deadBand)
	msg += fmt.Sprintf("L1: %+.0fW  L2: %+.0fW\n\n", d.GridL1, d.GridL2)

	// Per-inverter table
	for i, inv := range d.Inverters {
		if inv.Idled {
			msg += fmt.Sprintf("Inv%d SOC %d%% IDLE\n", i+1, inv.SOC)
		} else {
			msg += fmt.Sprintf("Inv%d SOC %d%% %+dW of %dW\n",
				i+1, inv.SOC, inv.ActualW, inv.TargetW)
		}
	}

	msg += fmt.Sprintf("Avg SOC: %d%%\n", d.AvgSOC)
	msg += fmt.Sprintf("Uptime:  %s", formatDuration(d.Uptime))

	return msg
}

// FormatModeChange formats a mode change alert
func FormatModeChange(state string, gridL1, gridL2 float32, soc int, detail string) string {
	now := time.Now().Format("15:04")
	return fmt.Sprintf(`→ %s at %s
  Grid: L1 %+.0fW L2 %+.0fW
  SOC: %d%%
  %s`,
		state, now, gridL1, gridL2, soc, detail)
}

// FormatNightReduced formats a night reduced alert
func FormatNightReduced(gridL1Before, gridL2Before, gridL1After, gridL2After float32, soc int, activeCount int, idled []byte) string {
	now := time.Now().Format("15:04")
	return fmt.Sprintf(`→ NIGHT_REDUCED at %s
  Grid: L1 %+.0fW L2 %+.0fW → L1 %+.0fW L2 %+.0fW
  SOC: %d%%
  Inverters: %d of 4 active (idled %v)`,
		now, gridL1Before, gridL2Before, gridL1After, gridL2After, soc, activeCount, idled)
}

// FormatBatteriesFull formats a batteries full alert
func FormatBatteriesFull(exportW float32, soc int) string {
	now := time.Now().Format("15:04")
	return fmt.Sprintf(`→ BATTERIES FULL at %s
  Grid: exporting %.1fkW
  SOC: %d%%
  Charge: stopped (XW Pro)`,
		now, -exportW/1000, soc)
}

// FormatFailure formats a failure alert
func FormatFailure(reason string, lastGridL1, lastGridL2 float32, lastSOC int) string {
	now := time.Now().Format("15:04")
	return fmt.Sprintf(`❌ SAFE at %s
  Last grid: L1 %+.0fW L2 %+.0fW
  Last SOC: %d%%
  Reason: %s`,
		now, lastGridL1, lastGridL2, lastSOC, reason)
}

// FormatRecovery formats a recovery alert
func FormatRecovery(state string, gridL1, gridL2 float32, soc int) string {
	now := time.Now().Format("15:04")
	return fmt.Sprintf(`✅ RECOVERED at %s
  Grid: L1 %+.0fW L2 %+.0fW
  SOC: %d%%
  Resuming: %s`,
		now, gridL1, gridL2, soc, state)
}

// FormatStopped formats a manual stop alert
func FormatStopped(gridL1, gridL2 float32, soc int) string {
	now := time.Now().Format("15:04")
	gridTotal := gridL1 + gridL2
	action := "importing"
	if gridTotal < 0 {
		action = "exporting"
		gridTotal = -gridTotal
	}
	return fmt.Sprintf(`→ STOPPED at %s (manual)
  Grid: %s %.1fkW
  SOC: %d%%
  Inverters: all idle`,
		now, action, gridTotal/1000, soc)
}

// FormatStarted formats a manual start alert
func FormatStarted(state string, gridL1, gridL2 float32, soc int) string {
	now := time.Now().Format("15:04")
	gridTotal := gridL1 + gridL2
	action := "importing"
	if gridTotal < 0 {
		action = "exporting"
		gridTotal = -gridTotal
	}
	return fmt.Sprintf(`→ STARTED at %s (manual)
  Grid: %s %.1fkW
  SOC: %d%%
  Mode: %s`,
		now, action, gridTotal/1000, soc, state)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60

	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

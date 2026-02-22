package telegram

import (
	"fmt"
	"time"
)

// AlertType constants for rate limiting
const (
	AlertModeChange = "mode_change"
	AlertFailure    = "failure"
	AlertRecovery   = "recovery"
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
	ChargeW        int
	DayDischarging bool
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
	if d.DayDischarging {
		msg += "Peak shave active\n"
	}
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

	msg += fmt.Sprintf("\nUptime:  %s", formatDuration(d.Uptime))

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

// FormatHelp returns the help text listing all commands
func FormatHelp() string {
	return `/status - Current state, grid, SOC, rates
/stop - Idle all inverters (manual)
/start - Resume normal operation
/up - Increase charge/discharge +300W/inv
/down - Decrease charge/discharge -300W/inv
/charge - Force charge mode
/discharge - Force discharge mode
/stats - Session statistics
/reboot - Reboot Insight gateway
/help - This message`
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60

	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// NightGuardEventData holds one night guard event for display
type NightGuardEventData struct {
	Time        time.Time
	UnitID      byte
	L1Before    float32
	L2Before    float32
	ActiveAfter int
}

// SessionStatsData holds session stats for telegram display
type SessionStatsData struct {
	Type      string
	StartTime time.Time
	EndTime   time.Time
	StartSOC  int
	EndSOC    int

	TotalEnergyWh float64
	GridEnergyWh  float64
	AvgGridPowerW float64

	// Charge fields
	PeakChargePerInvW    int
	PeakChargeTotalW     int
	RampUpCount          int
	RampDownCount        int
	TimeAtMaxSec         float64
	MaxRollingExport5min float64
	MaxRollingImport5min float64

	// Discharge fields
	MaxRollingExport1min float64
	NightGuardEvents     []NightGuardEventData
}

// FormatStats formats the /stats response from current, lastCharge, lastDischarge
func FormatStats(current, lastCharge, lastDischarge *SessionStatsData) string {
	if current == nil && lastCharge == nil && lastDischarge == nil {
		return "No session data yet"
	}

	msg := ""
	if current != nil {
		msg += formatSessionBlock("CURRENT (in progress)", current)
	}
	if lastCharge != nil {
		if msg != "" {
			msg += "\n"
		}
		msg += formatSessionBlock("LAST CHARGE (completed)", lastCharge)
	}
	if lastDischarge != nil {
		if msg != "" {
			msg += "\n"
		}
		msg += formatSessionBlock("LAST DISCHARGE (completed)", lastDischarge)
	}
	return msg
}

func formatSessionBlock(label string, s *SessionStatsData) string {
	msg := fmt.Sprintf("-- %s --\n", label)
	msg += fmt.Sprintf("  %s  %s\n", s.Type, s.StartTime.Format("15:04"))

	if !s.EndTime.IsZero() {
		msg += fmt.Sprintf("  End: %s\n", s.EndTime.Format("15:04"))
	}

	dur := s.EndTime.Sub(s.StartTime)
	if s.EndTime.IsZero() {
		dur = time.Since(s.StartTime)
	}
	msg += fmt.Sprintf("  Duration: %s\n", formatDuration(dur))
	msg += fmt.Sprintf("  SOC: %d%% -> %d%%\n", s.StartSOC, s.EndSOC)
	msg += fmt.Sprintf("  Energy: %.1f kWh\n", s.TotalEnergyWh/1000)

	// Grid energy: negative = net export
	gridKWh := s.GridEnergyWh / 1000
	msg += fmt.Sprintf("  Grid: %.1f kWh (avg %+.0fW)\n", gridKWh, s.AvgGridPowerW)

	if s.Type == "CHARGE" {
		if s.PeakChargeTotalW > 0 {
			msg += fmt.Sprintf("  Peak: %dW/inv (%dW total)\n", s.PeakChargePerInvW, s.PeakChargeTotalW)
		}
		msg += fmt.Sprintf("  Ramps: %d up / %d down\n", s.RampUpCount, s.RampDownCount)
		if s.TimeAtMaxSec > 0 {
			msg += fmt.Sprintf("  At max: %s\n", formatDuration(time.Duration(s.TimeAtMaxSec)*time.Second))
		}
		if s.MaxRollingExport5min > 0 {
			msg += fmt.Sprintf("  Max 5m avg export: %.1fkW\n", s.MaxRollingExport5min/1000)
		}
		if s.MaxRollingImport5min > 0 {
			msg += fmt.Sprintf("  Max 5m avg import: %.1fkW\n", s.MaxRollingImport5min/1000)
		}
	}

	if s.Type == "DISCHARGE" {
		if s.MaxRollingExport1min > 0 {
			msg += fmt.Sprintf("  Max 1m avg export: %.0fW\n", s.MaxRollingExport1min)
		}
		if len(s.NightGuardEvents) > 0 {
			msg += fmt.Sprintf("  Guard events: %d\n", len(s.NightGuardEvents))
			for _, ev := range s.NightGuardEvents {
				msg += fmt.Sprintf("    %s inv%d L1:%+.0f L2:%+.0f -> %d active\n",
					ev.Time.Format("15:04"), ev.UnitID, ev.L1Before, ev.L2Before, ev.ActiveAfter)
			}
		}
	}

	return msg
}

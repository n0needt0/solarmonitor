package controller

import (
	"time"
)

// Scheduler handles time-based state transitions
type Scheduler struct {
	chargeStartHour int
	chargeEndHour   int
}

// NewScheduler creates a scheduler with charge window hours
func NewScheduler(chargeStart, chargeEnd int) *Scheduler {
	return &Scheduler{
		chargeStartHour: chargeStart,
		chargeEndHour:   chargeEnd,
	}
}

// IsChargeWindow returns true if current time is within charge window
func (s *Scheduler) IsChargeWindow() bool {
	hour := time.Now().Hour()
	return hour >= s.chargeStartHour && hour < s.chargeEndHour
}

// DesiredState returns what state we should be in based on time
func (s *Scheduler) DesiredState() State {
	if s.IsChargeWindow() {
		return StateDayCharge
	}
	return StateNightDischarge
}

// ChargeWindowString returns human readable charge window
func (s *Scheduler) ChargeWindowString() string {
	return formatHour(s.chargeStartHour) + "-" + formatHour(s.chargeEndHour)
}

func formatHour(h int) string {
	if h == 0 {
		return "12am"
	} else if h < 12 {
		return string(rune('0'+h/10)) + string(rune('0'+h%10)) + "am"
	} else if h == 12 {
		return "12pm"
	} else {
		h -= 12
		return string(rune('0'+h/10)) + string(rune('0'+h%10)) + "pm"
	}
}

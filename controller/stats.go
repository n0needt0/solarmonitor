package controller

import (
	"math"
	"sync"
	"time"
)

// SessionType identifies charge vs discharge sessions
type SessionType int

const (
	SessionCharge    SessionType = iota
	SessionDischarge
)

func (s SessionType) String() string {
	if s == SessionCharge {
		return "CHARGE"
	}
	return "DISCHARGE"
}

// NightGuardEvent records a single inverter idle event during night guard
type NightGuardEvent struct {
	Time        time.Time
	UnitID      byte
	L1Before    float32
	L2Before    float32
	ActiveAfter int
}

// SessionStats holds all accumulators for one charge or discharge session
type SessionStats struct {
	Type      SessionType
	StartTime time.Time
	EndTime   time.Time
	StartSOC  int
	EndSOC    int

	// Battery energy from BMS power integration (Wh)
	TotalEnergyWh float64

	// Grid energy from grid power integration (Wh, negative = net export)
	GridEnergyWh float64
	GridSampleCount int

	// Charge-specific
	PeakChargePerInvW int
	PeakChargeTotalW  int
	RampUpCount       int
	RampDownCount     int
	TimeAtMaxSec      float64
	MaxRollingExport5min float64 // max of 5-min rolling avg export (W, positive = export)
	MaxRollingImport5min float64 // max of 5-min rolling avg import (W, positive = import)

	// Discharge-specific
	MaxRollingExport1min float64 // max of 1-min rolling avg export (W, positive = export)
	NightGuardEvents     []NightGuardEvent
}

// AvgGridPowerW returns the average grid power over the session
func (s *SessionStats) AvgGridPowerW() float64 {
	if s.GridSampleCount == 0 {
		return 0
	}
	dur := s.duration().Seconds()
	if dur <= 0 {
		return 0
	}
	return s.GridEnergyWh * 3600 / dur
}

func (s *SessionStats) duration() time.Duration {
	end := s.EndTime
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(s.StartTime)
}

// ringEntry stores one grid sample
type ringEntry struct {
	totalW float32
	at     time.Time
}

// RingBuffer holds grid samples for rolling average calculations.
// 30 entries at 10s intervals = 5 minutes.
const ringSize = 30

// RingBuffer is a fixed-size circular buffer of grid readings
type RingBuffer struct {
	entries [ringSize]ringEntry
	pos     int
	count   int
}

// Add inserts a new grid sample
func (rb *RingBuffer) Add(totalW float32, at time.Time) {
	rb.entries[rb.pos] = ringEntry{totalW: totalW, at: at}
	rb.pos = (rb.pos + 1) % ringSize
	if rb.count < ringSize {
		rb.count++
	}
}

// AvgOver returns the average grid power over the given duration looking back from the most recent entry.
// Returns (average, ok). ok is false if no samples exist in the window.
func (rb *RingBuffer) AvgOver(d time.Duration) (float64, bool) {
	if rb.count == 0 {
		return 0, false
	}

	// Most recent entry
	latest := (rb.pos - 1 + ringSize) % ringSize
	cutoff := rb.entries[latest].at.Add(-d)

	var sum float64
	var n int
	for i := 0; i < rb.count; i++ {
		idx := (rb.pos - 1 - i + ringSize*2) % ringSize
		e := rb.entries[idx]
		if e.at.Before(cutoff) {
			break
		}
		sum += float64(e.totalW)
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// StatsTracker is the thread-safe wrapper for session statistics
type StatsTracker struct {
	mu            sync.RWMutex
	current       *SessionStats
	lastCharge    *SessionStats
	lastDischarge *SessionStats
	maxPerInvW    int

	// Ring buffer for rolling grid averages
	ring          RingBuffer
	lastGridAt    time.Time
}

// NewStatsTracker creates a new stats tracker
func NewStatsTracker(maxPerInvW int) *StatsTracker {
	return &StatsTracker{
		maxPerInvW: maxPerInvW,
	}
}

// StartSession finalizes any current session and starts a new one
func (t *StatsTracker) StartSession(st SessionType, soc int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Finalize current session if exists
	t.finalizeLocked(soc)

	t.current = &SessionStats{
		Type:      st,
		StartTime: time.Now(),
		StartSOC:  soc,
	}
	// Reset ring buffer for new session
	t.ring = RingBuffer{}
}

// EndSession finalizes the current session
func (t *StatsTracker) EndSession(soc int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finalizeLocked(soc)
}

// finalizeLocked moves the current session to lastCharge/lastDischarge. Caller holds lock.
func (t *StatsTracker) finalizeLocked(soc int) {
	if t.current == nil {
		return
	}
	t.current.EndTime = time.Now()
	t.current.EndSOC = soc
	if t.current.Type == SessionCharge {
		t.lastCharge = t.current
	} else {
		t.lastDischarge = t.current
	}
	t.current = nil
}

// RecordGridReading records a grid power sample for energy integration and rolling averages.
// totalW follows the convention: positive = importing, negative = exporting.
func (t *StatsTracker) RecordGridReading(totalW float32, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return
	}

	// Energy integration: W * seconds / 3600 = Wh
	if !t.lastGridAt.IsZero() {
		dt := at.Sub(t.lastGridAt).Seconds()
		if dt > 0 && dt < 60 { // skip if gap is too large
			t.current.GridEnergyWh += float64(totalW) * dt / 3600.0
			t.current.GridSampleCount++
		}
	}
	t.lastGridAt = at

	// Ring buffer
	t.ring.Add(totalW, at)

	// Update rolling peak averages
	if t.current.Type == SessionCharge {
		// 5-min rolling averages for charge sessions
		if avg, ok := t.ring.AvgOver(5 * time.Minute); ok {
			if avg < 0 { // exporting
				exportAvg := -avg
				if exportAvg > t.current.MaxRollingExport5min {
					t.current.MaxRollingExport5min = exportAvg
				}
			} else { // importing
				if avg > t.current.MaxRollingImport5min {
					t.current.MaxRollingImport5min = avg
				}
			}
		}
	} else {
		// 1-min rolling average for discharge sessions
		if avg, ok := t.ring.AvgOver(1 * time.Minute); ok {
			if avg < 0 { // exporting
				exportAvg := -avg
				if exportAvg > t.current.MaxRollingExport1min {
					t.current.MaxRollingExport1min = exportAvg
				}
			}
		}
	}
}

// RecordChargeRate records a charge rate change
func (t *StatsTracker) RecordChargeRate(perInvW int, isRampUp bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return
	}

	if isRampUp {
		t.current.RampUpCount++
	} else {
		t.current.RampDownCount++
	}

	totalW := perInvW * 4
	if perInvW > t.current.PeakChargePerInvW {
		t.current.PeakChargePerInvW = perInvW
	}
	if totalW > t.current.PeakChargeTotalW {
		t.current.PeakChargeTotalW = totalW
	}
}

// RecordChargeEnergy integrates battery power for energy tracking.
// totalBatteryPowerW is the sum of all inverter battery power (positive = charging).
func (t *StatsTracker) RecordChargeEnergy(totalBatteryPowerW int, intervalSec float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return
	}

	wh := math.Abs(float64(totalBatteryPowerW)) * intervalSec / 3600.0
	t.current.TotalEnergyWh += wh
}

// RecordMaxRateTime accumulates time spent at maximum charge rate
func (t *StatsTracker) RecordMaxRateTime(perInvW int, intervalSec float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return
	}

	if perInvW >= t.maxPerInvW {
		t.current.TimeAtMaxSec += intervalSec
	}
}

// RecordNightGuardEvent records a night guard inverter idle event
func (t *StatsTracker) RecordNightGuardEvent(unitID byte, l1, l2 float32, activeAfter int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return
	}

	t.current.NightGuardEvents = append(t.current.NightGuardEvents, NightGuardEvent{
		Time:        time.Now(),
		UnitID:      unitID,
		L1Before:    l1,
		L2Before:    l2,
		ActiveAfter: activeAfter,
	})
}

// CurrentSession returns a copy of the current session (nil if none)
func (t *StatsTracker) CurrentSession() *SessionStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.current == nil {
		return nil
	}
	cp := *t.current
	// Deep copy night guard events
	if len(t.current.NightGuardEvents) > 0 {
		cp.NightGuardEvents = make([]NightGuardEvent, len(t.current.NightGuardEvents))
		copy(cp.NightGuardEvents, t.current.NightGuardEvents)
	}
	return &cp
}

// LastCharge returns a copy of the last completed charge session (nil if none)
func (t *StatsTracker) LastCharge() *SessionStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.lastCharge == nil {
		return nil
	}
	cp := *t.lastCharge
	if len(t.lastCharge.NightGuardEvents) > 0 {
		cp.NightGuardEvents = make([]NightGuardEvent, len(t.lastCharge.NightGuardEvents))
		copy(cp.NightGuardEvents, t.lastCharge.NightGuardEvents)
	}
	return &cp
}

// LastDischarge returns a copy of the last completed discharge session (nil if none)
func (t *StatsTracker) LastDischarge() *SessionStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.lastDischarge == nil {
		return nil
	}
	cp := *t.lastDischarge
	if len(t.lastDischarge.NightGuardEvents) > 0 {
		cp.NightGuardEvents = make([]NightGuardEvent, len(t.lastDischarge.NightGuardEvents))
		copy(cp.NightGuardEvents, t.lastDischarge.NightGuardEvents)
	}
	return &cp
}

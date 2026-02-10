package controller

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State represents the controller's operating state
type State int

const (
	StateIdle State = iota
	StateDayCharge
	StateNightDischarge
	StateNightReduced
	StateStopped
	StateSafe
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "IDLE"
	case StateDayCharge:
		return "DAY_CHARGE"
	case StateNightDischarge:
		return "NIGHT_DISCHARGE"
	case StateNightReduced:
		return "NIGHT_REDUCED"
	case StateStopped:
		return "STOPPED"
	case StateSafe:
		return "SAFE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", s)
	}
}

// StateManager tracks current state and handles transitions
type StateManager struct {
	mu            sync.RWMutex
	current       State
	previous      State
	enteredAt     time.Time
	stateChangeCh chan StateChange

	// Night reduced tracking
	idledInverters []byte // Unit IDs that have been idled
}

// StateChange is sent when state transitions
type StateChange struct {
	From      State
	To        State
	Reason    string
	Timestamp time.Time
}

// NewStateManager creates a new state manager starting in IDLE
func NewStateManager() *StateManager {
	return &StateManager{
		current:        StateIdle,
		enteredAt:      time.Now(),
		stateChangeCh:  make(chan StateChange, 10),
		idledInverters: make([]byte, 0, 4),
	}
}

// Current returns the current state
func (sm *StateManager) Current() State {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current
}

// TimeInState returns how long we've been in the current state
func (sm *StateManager) TimeInState() time.Duration {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return time.Since(sm.enteredAt)
}

// Transition changes to a new state with reason
func (sm *StateManager) Transition(newState State, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if newState == sm.current {
		return // No change
	}

	change := StateChange{
		From:      sm.current,
		To:        newState,
		Reason:    reason,
		Timestamp: time.Now(),
	}

	slog.Info("state_change",
		"from", sm.current.String(),
		"to", newState.String(),
		"reason", reason,
	)

	sm.previous = sm.current
	sm.current = newState
	sm.enteredAt = time.Now()

	// Reset night reduced tracking on day transition
	if newState == StateDayCharge {
		sm.idledInverters = sm.idledInverters[:0]
	}

	// Non-blocking send
	select {
	case sm.stateChangeCh <- change:
	default:
		slog.Warn("state_change_channel_full")
	}
}

// IdledInverters returns list of inverters idled during night reduced
func (sm *StateManager) IdledInverters() []byte {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]byte, len(sm.idledInverters))
	copy(result, sm.idledInverters)
	return result
}

// AddIdledInverter records an inverter being idled
func (sm *StateManager) AddIdledInverter(unitID byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.idledInverters = append(sm.idledInverters, unitID)
}

// StateChangeCh returns the channel that receives state changes
func (sm *StateManager) StateChangeCh() <-chan StateChange {
	return sm.stateChangeCh
}

package insight

import (
	"fmt"
	"log/slog"
)

// XW Pro Modbus registers
const (
	RegEPCChargeMax    uint16 = 40210 // EPC Charge Max Power (watts)
	RegEPCModeCommand  uint16 = 40213 // EPC Mode: 0=idle, 1=charge
	RegEPCMaxDischarge uint16 = 40152 // EPC Max Discharge Power (watts)
	RegRechargeSOC     uint16 = 40149 // Recharge SOC (% × 10)
)

// EPC mode values
const (
	EPCModeIdle   uint16 = 0
	EPCModeCharge uint16 = 1
)

// InverterState holds the current state of an inverter
type InverterState struct {
	UnitID        byte
	ChargeLimit   uint16 // watts
	EPCMode       uint16 // 0=idle, 1=charge
	DischargeMax  uint16 // watts
	RechargeSOC   uint16 // % × 10
}

// SetChargeMode sets an inverter to charge mode with specified power limit
func (c *Client) SetChargeMode(unitID byte, powerW uint16) error {
	slog.Info("set_charge_mode", "unit", unitID, "power_w", powerW)

	// Set charge limit first
	if err := c.WriteRegister(unitID, RegEPCChargeMax, powerW); err != nil {
		return fmt.Errorf("set charge limit: %w", err)
	}

	// Then enable charge mode
	if err := c.WriteRegister(unitID, RegEPCModeCommand, EPCModeCharge); err != nil {
		return fmt.Errorf("set charge mode: %w", err)
	}

	return nil
}

// SetIdleMode sets an inverter to idle (no charge/discharge)
func (c *Client) SetIdleMode(unitID byte) error {
	slog.Info("set_idle_mode", "unit", unitID)

	if err := c.WriteRegister(unitID, RegEPCModeCommand, EPCModeIdle); err != nil {
		return fmt.Errorf("set idle mode: %w", err)
	}

	return nil
}

// SetDischargeLimit sets the maximum discharge power for an inverter
func (c *Client) SetDischargeLimit(unitID byte, powerW uint16) error {
	slog.Info("set_discharge_limit", "unit", unitID, "power_w", powerW)

	if err := c.WriteRegister(unitID, RegEPCMaxDischarge, powerW); err != nil {
		return fmt.Errorf("set discharge limit: %w", err)
	}

	return nil
}

// SetRechargeSOC sets the recharge SOC threshold
func (c *Client) SetRechargeSOC(unitID byte, socPercent int) error {
	slog.Info("set_recharge_soc", "unit", unitID, "soc_pct", socPercent)

	// Register expects % × 10
	value := uint16(socPercent * 10)
	if err := c.WriteRegister(unitID, RegRechargeSOC, value); err != nil {
		return fmt.Errorf("set recharge SOC: %w", err)
	}

	return nil
}

// ReadInverterState reads current state from an inverter
func (c *Client) ReadInverterState(unitID byte) (*InverterState, error) {
	state := &InverterState{UnitID: unitID}

	var err error

	state.ChargeLimit, err = c.ReadRegister(unitID, RegEPCChargeMax)
	if err != nil {
		return nil, fmt.Errorf("read charge limit: %w", err)
	}

	state.EPCMode, err = c.ReadRegister(unitID, RegEPCModeCommand)
	if err != nil {
		return nil, fmt.Errorf("read EPC mode: %w", err)
	}

	return state, nil
}

// IdleAllInverters sets all inverters to idle mode
func (c *Client) IdleAllInverters(unitIDs []byte) error {
	for _, id := range unitIDs {
		if err := c.SetIdleMode(id); err != nil {
			return fmt.Errorf("idle unit %d: %w", id, err)
		}
	}
	return nil
}

// SetAllChargeMode sets all inverters to charge mode with specified power
func (c *Client) SetAllChargeMode(unitIDs []byte, powerW uint16) error {
	for _, id := range unitIDs {
		if err := c.SetChargeMode(id, powerW); err != nil {
			return fmt.Errorf("charge unit %d: %w", id, err)
		}
	}
	return nil
}

// SetAllDischargeLimit sets discharge limit on all inverters
func (c *Client) SetAllDischargeLimit(unitIDs []byte, powerW uint16) error {
	for _, id := range unitIDs {
		if err := c.SetDischargeLimit(id, powerW); err != nil {
			return fmt.Errorf("discharge limit unit %d: %w", id, err)
		}
	}
	return nil
}

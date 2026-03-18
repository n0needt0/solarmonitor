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
	EPCModeIdle      uint16 = 0
	EPCModeCharge    uint16 = 1
	EPCModeDischarge uint16 = 2
)

// SetChargeMode sets an inverter to charge mode with specified power limit
func (c *Client) SetChargeMode(unitID byte, powerW uint16) error {
	slog.Debug("set_charge_mode", "unit", unitID, "power_w", powerW)

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
	slog.Debug("set_idle_mode", "unit", unitID)

	if err := c.WriteRegister(unitID, RegEPCModeCommand, EPCModeIdle); err != nil {
		return fmt.Errorf("set idle mode: %w", err)
	}

	return nil
}

// SetDischargeMode sets an inverter to discharge mode with specified power limit
func (c *Client) SetDischargeMode(unitID byte, powerW uint16) error {
	slog.Debug("set_discharge_mode", "unit", unitID, "power_w", powerW)

	// Set EPC mode to discharge (2)
	if err := c.WriteRegister(unitID, RegEPCModeCommand, EPCModeDischarge); err != nil {
		return fmt.Errorf("set discharge mode: %w", err)
	}

	// Set discharge limit
	if err := c.WriteRegister(unitID, RegEPCMaxDischarge, powerW); err != nil {
		return fmt.Errorf("set discharge limit: %w", err)
	}

	return nil
}


// SetDischargePower updates only the discharge power limit (no mode write).
// Used for keepalives where mode is already set.
func (c *Client) SetDischargePower(unitID byte, powerW uint16) error {
	return c.WriteRegister(unitID, RegEPCMaxDischarge, powerW)
}

// SetChargePower updates only the charge power limit (no mode write).
// Used for keepalives where mode is already set.
func (c *Client) SetChargePower(unitID byte, powerW uint16) error {
	return c.WriteRegister(unitID, RegEPCChargeMax, powerW)
}

// ReadEPCStatus reads current EPC mode and power limits from port 503 (read port).
func (c *Client) ReadEPCStatus(unitID byte) (mode uint16, chargeW uint16, dischargeW uint16, err error) {
	mode, err = c.ReadRegister(unitID, RegEPCModeCommand)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read EPC mode: %w", err)
	}
	chargeW, err = c.ReadRegister(unitID, RegEPCChargeMax)
	if err != nil {
		return mode, 0, 0, fmt.Errorf("read charge max: %w", err)
	}
	dischargeW, err = c.ReadRegister(unitID, RegEPCMaxDischarge)
	if err != nil {
		return mode, chargeW, 0, fmt.Errorf("read discharge max: %w", err)
	}
	return mode, chargeW, dischargeW, nil
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


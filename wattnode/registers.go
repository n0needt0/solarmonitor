package wattnode

// WattNode Modbus register addresses (input registers, function code 4)
// Reference: https://ctlsys.com/support/wattnode-modbus-register-map/
const (
	// Energy registers (Float32, kWh)
	RegEnergySum uint16 = 1001 // Total positive energy
	RegEnergyA   uint16 = 1003 // Phase A positive energy
	RegEnergyB   uint16 = 1005 // Phase B positive energy

	// Power registers (Float32, Watts)
	RegPowerA   uint16 = 1009 // Phase A (L1) Power in Watts
	RegPowerB   uint16 = 1011 // Phase B (L2) Power in Watts
	RegPowerSum uint16 = 1015 // Total Power in Watts

	// Voltage registers (Float32, Volts)
	RegVoltageA uint16 = 1017 // Phase A Voltage
	RegVoltageB uint16 = 1019 // Phase B Voltage

	// Configuration registers (holding registers, function code 3)
	// Addresses confirmed from raw dump of 1602-1611
	RegCtAmpsA        uint16 = 1603 // CT rated amps Phase A (uint16)
	RegCtAmpsB        uint16 = 1604 // CT rated amps Phase B (uint16)
	RegCtAmpsC        uint16 = 1605 // CT rated amps Phase C (uint16)
	RegConnectionType uint16 = 1610 // 1=1P2W, 2=1P3W (split-phase), 3=3P4W
	RegConfigStart    uint16 = 1602 // Start of config block for bulk read
)

// GridPower holds per-leg and total grid power readings
// Positive = importing from grid, Negative = exporting to grid
type GridPower struct {
	L1    float32 // Phase A power (watts)
	L2    float32 // Phase B power (watts)
	Total float32 // Total power (watts)
}

// IsExporting returns true if either leg is exporting
func (g *GridPower) IsExporting() bool {
	return g.L1 < 0 || g.L2 < 0
}

// ExportAmount returns total export (positive number) or 0 if importing
func (g *GridPower) ExportAmount() float32 {
	if g.Total < 0 {
		return -g.Total
	}
	return 0
}

// ImportAmount returns total import (positive number) or 0 if exporting
func (g *GridPower) ImportAmount() float32 {
	if g.Total > 0 {
		return g.Total
	}
	return 0
}

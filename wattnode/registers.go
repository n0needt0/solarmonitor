package wattnode

// WattNode Modbus register addresses
// Reference: https://ctlsys.com/support/wattnode-modbus-register-map/
const (
	// Power registers (Float32: Big Endian bytes, Little Endian words)
	RegPowerA   uint16 = 1009 // Phase A (L1) Power in Watts
	RegPowerB   uint16 = 1011 // Phase B (L2) Power in Watts
	RegPowerSum uint16 = 1015 // Total Power in Watts

	// Voltage registers
	RegVoltageA uint16 = 1017 // Phase A Voltage
	RegVoltageB uint16 = 1019 // Phase B Voltage
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

package insight

import (
	"fmt"
)

// BMS bulk read registers (port 503, unit ID 1)
const (
	RegBMSBulkStart uint16 = 960
	RegBMSBulkCount uint16 = 42 // 960-1001
)

// BMS data offsets within bulk read (from register 960)
// Power registers: 967, 977, 987, 997 = indices 7, 17, 27, 37
// SOC registers:   969, 979, 989, 999 = indices 9, 19, 29, 39
const (
	OffsetInv1Power = 7
	OffsetInv1SOC   = 9
	OffsetInv2Power = 17
	OffsetInv2SOC   = 19
	OffsetInv3Power = 27
	OffsetInv3SOC   = 29
	OffsetInv4Power = 37
	OffsetInv4SOC   = 39
)

// BatteryStatus holds SOC and power for all 4 inverters
type BatteryStatus struct {
	SOC   [4]int   // SOC percentage for each inverter
	Power [4]int16 // Battery power in watts (positive=charging, negative=discharging)
}

// TotalSOC returns average SOC across all batteries
func (b *BatteryStatus) TotalSOC() int {
	sum := 0
	for _, s := range b.SOC {
		sum += s
	}
	return sum / 4
}

// TotalPower returns sum of all battery power
func (b *BatteryStatus) TotalPower() int {
	sum := 0
	for _, p := range b.Power {
		sum += int(p)
	}
	return sum
}

// ReadBatteryStatus reads SOC and power for all 4 battery banks
func (c *Client) ReadBatteryStatus() (*BatteryStatus, error) {
	// Bulk read from unit ID 1 (BMS gateway) - holding registers on port 503
	data, err := c.ReadHoldingRegisters(1, RegBMSBulkStart, RegBMSBulkCount)
	if err != nil {
		return nil, fmt.Errorf("bulk BMS read: %w", err)
	}

	status := &BatteryStatus{}

	// Extract SOC values
	status.SOC[0] = int(data[OffsetInv1SOC])
	status.SOC[1] = int(data[OffsetInv2SOC])
	status.SOC[2] = int(data[OffsetInv3SOC])
	status.SOC[3] = int(data[OffsetInv4SOC])

	// Extract power values (signed 16-bit)
	status.Power[0] = toSigned16(data[OffsetInv1Power])
	status.Power[1] = toSigned16(data[OffsetInv2Power])
	status.Power[2] = toSigned16(data[OffsetInv3Power])
	status.Power[3] = toSigned16(data[OffsetInv4Power])

	return status, nil
}

// toSigned16 converts unsigned 16-bit to signed
func toSigned16(val uint16) int16 {
	return int16(val)
}

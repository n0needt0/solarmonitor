package wattnode

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/goburrow/modbus"
)

// Reader handles USB Modbus RTU communication with WattNode meter
type Reader struct {
	handler     *modbus.RTUClientHandler
	client      modbus.Client
	unitID      byte
	scaleFactor float32
	mu          sync.Mutex

	// Latest reading
	lastPower GridPower
	lastRead  time.Time
	lastErr   error

	// Failure tracking
	consecutiveFailures int
}

// NewReader creates a WattNode reader for USB serial connection
func NewReader(port string, baud int, unitID byte, scaleFactor float32) (*Reader, error) {
	if scaleFactor == 0 {
		scaleFactor = 1.0
	}
	handler := modbus.NewRTUClientHandler(port)
	handler.BaudRate = baud
	handler.DataBits = 8
	handler.Parity = "N"
	handler.StopBits = 1
	handler.SlaveId = unitID
	handler.Timeout = 2 * time.Second

	if err := handler.Connect(); err != nil {
		return nil, fmt.Errorf("connect to %s: %w", port, err)
	}

	return &Reader{
		handler:     handler,
		client:      modbus.NewClient(handler),
		unitID:      unitID,
		scaleFactor: scaleFactor,
	}, nil
}

// Close closes the serial connection
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handler.Close()
}

// Read fetches current grid power from WattNode
func (r *Reader) Read() (*GridPower, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Read L1 power (2 registers for Float32)
	// WattNode register addresses are used directly (no offset)
	l1Data, err := r.client.ReadInputRegisters(RegPowerA, 2)
	if err != nil {
		r.consecutiveFailures++
		r.lastErr = err
		return nil, fmt.Errorf("read L1 power: %w", err)
	}

	// Read L2 power
	l2Data, err := r.client.ReadInputRegisters(RegPowerB, 2)
	if err != nil {
		r.consecutiveFailures++
		r.lastErr = err
		return nil, fmt.Errorf("read L2 power: %w", err)
	}

	// Parse Float32 values (standard Big Endian)
	// Negate values - USB WattNode CTs have reversed polarity
	// Convention: Positive = importing, Negative = exporting
	// Apply scale factor for CT ratio calibration
	l1 := -parseFloat32BE(l1Data) * r.scaleFactor
	l2 := -parseFloat32BE(l2Data) * r.scaleFactor
	// Total register (1015) not reliable on this meter - calculate from L1+L2
	r.lastPower = GridPower{
		L1:    l1,
		L2:    l2,
		Total: l1 + l2,
	}
	r.lastRead = time.Now()
	r.lastErr = nil
	r.consecutiveFailures = 0

	return &r.lastPower, nil
}

// DiagConfig holds WattNode voltage readings for diagnostics
type DiagConfig struct {
	VoltageA float32
	VoltageB float32
}

// ReadDiagConfig reads WattNode voltage registers for diagnostics
func (r *Reader) ReadDiagConfig() (*DiagConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg := &DiagConfig{}

	vData, err := r.client.ReadInputRegisters(RegVoltageA, 2)
	if err == nil {
		cfg.VoltageA = parseFloat32BE(vData)
	}
	vData, err = r.client.ReadInputRegisters(RegVoltageB, 2)
	if err == nil {
		cfg.VoltageB = parseFloat32BE(vData)
	}

	return cfg, nil
}

// LastReading returns the most recent reading without making a new request
func (r *Reader) LastReading() (*GridPower, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &r.lastPower, r.lastRead
}

// ConsecutiveFailures returns the number of consecutive read failures
func (r *Reader) ConsecutiveFailures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consecutiveFailures
}

// LastError returns the last error encountered
func (r *Reader) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// parseFloat32BE parses a Float32 in standard Big Endian format
// WattNode returns registers in order [high_word, low_word]
// Data bytes come as: [r0_hi, r0_lo, r1_hi, r1_lo] = standard BE float
func parseFloat32BE(data []byte) float32 {
	if len(data) != 4 {
		return 0
	}
	bits := binary.BigEndian.Uint32(data)
	return math.Float32frombits(bits)
}

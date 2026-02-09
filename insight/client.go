package insight

import (
	"fmt"
	"sync"
	"time"

	"github.com/goburrow/modbus"
)

// Client handles Modbus TCP communication with Insight Facility gateway
type Client struct {
	host      string
	readPort  int
	writePort int
	minGapMs  int
	timeoutMs int

	readHandler  *modbus.TCPClientHandler
	writeHandler *modbus.TCPClientHandler
	readClient   modbus.Client
	writeClient  modbus.Client

	mu           sync.Mutex
	lastWriteAt  time.Time
	connected    bool
	lastErr      error
}

// NewClient creates an Insight Modbus TCP client
func NewClient(host string, readPort, writePort, minGapMs, timeoutMs int) *Client {
	return &Client{
		host:      host,
		readPort:  readPort,
		writePort: writePort,
		minGapMs:  minGapMs,
		timeoutMs: timeoutMs,
	}
}

// Connect establishes connections to read and write ports
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Read port (503)
	c.readHandler = modbus.NewTCPClientHandler(fmt.Sprintf("%s:%d", c.host, c.readPort))
	c.readHandler.Timeout = time.Duration(c.timeoutMs) * time.Millisecond
	if err := c.readHandler.Connect(); err != nil {
		return fmt.Errorf("connect read port %d: %w", c.readPort, err)
	}
	c.readClient = modbus.NewClient(c.readHandler)

	// Write port (502)
	c.writeHandler = modbus.NewTCPClientHandler(fmt.Sprintf("%s:%d", c.host, c.writePort))
	c.writeHandler.Timeout = time.Duration(c.timeoutMs) * time.Millisecond
	if err := c.writeHandler.Connect(); err != nil {
		c.readHandler.Close()
		return fmt.Errorf("connect write port %d: %w", c.writePort, err)
	}
	c.writeClient = modbus.NewClient(c.writeHandler)

	c.connected = true
	return nil
}

// Close closes both connections
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	if c.readHandler != nil {
		if err := c.readHandler.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.writeHandler != nil {
		if err := c.writeHandler.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	c.connected = false

	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// waitForGap ensures minimum time between writes
func (c *Client) waitForGap() {
	elapsed := time.Since(c.lastWriteAt)
	required := time.Duration(c.minGapMs) * time.Millisecond
	if elapsed < required {
		time.Sleep(required - elapsed)
	}
}

// ReadRegister reads a single holding register from an inverter
// Insight gateway uses full register address
func (c *Client) ReadRegister(unitID byte, register uint16) (uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.readHandler.SlaveId = unitID
	data, err := c.readClient.ReadHoldingRegisters(register, 1)
	if err != nil {
		c.lastErr = err
		return 0, fmt.Errorf("read register %d from unit %d: %w", register, unitID, err)
	}

	if len(data) < 2 {
		return 0, fmt.Errorf("short response from unit %d", unitID)
	}

	return uint16(data[0])<<8 | uint16(data[1]), nil
}

// ReadHoldingRegisters reads multiple consecutive holding registers
// Insight gateway uses full register address
func (c *Client) ReadHoldingRegisters(unitID byte, startRegister uint16, count uint16) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.readHandler.SlaveId = unitID
	data, err := c.readClient.ReadHoldingRegisters(startRegister, count)
	if err != nil {
		c.lastErr = err
		return nil, fmt.Errorf("read holding registers %d-%d from unit %d: %w", startRegister, startRegister+count-1, unitID, err)
	}

	result := make([]uint16, count)
	for i := uint16(0); i < count; i++ {
		result[i] = uint16(data[i*2])<<8 | uint16(data[i*2+1])
	}
	return result, nil
}

// ReadInputRegisters reads multiple consecutive input registers
// Used for BMS data (registers 960+)
func (c *Client) ReadInputRegisters(unitID byte, startRegister uint16, count uint16) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.readHandler.SlaveId = unitID
	data, err := c.readClient.ReadInputRegisters(startRegister, count)
	if err != nil {
		c.lastErr = err
		return nil, fmt.Errorf("read input registers %d-%d from unit %d: %w", startRegister, startRegister+count-1, unitID, err)
	}

	result := make([]uint16, count)
	for i := uint16(0); i < count; i++ {
		result[i] = uint16(data[i*2])<<8 | uint16(data[i*2+1])
	}
	return result, nil
}

// WriteRegister writes a single holding register to an inverter
// Insight gateway uses full register address (40213 not offset 212)
func (c *Client) WriteRegister(unitID byte, register uint16, value uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.waitForGap()

	c.writeHandler.SlaveId = unitID
	// Insight uses full register address
	_, err := c.writeClient.WriteSingleRegister(register, value)
	c.lastWriteAt = time.Now()

	if err != nil {
		c.lastErr = err
		return fmt.Errorf("write register %d = %d to unit %d: %w", register, value, unitID, err)
	}

	return nil
}

// IsConnected returns connection status
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// LastError returns the last error encountered
func (c *Client) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

package insight

import (
	"fmt"
	"log/slog"
	"strings"
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

	mu          sync.Mutex
	lastWriteAt time.Time
	connected   bool
	lastErr     error
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

	return c.connectLocked()
}

// connectLocked does the actual connection (must hold lock)
func (c *Client) connectLocked() error {
	// Close existing connections if any
	if c.readHandler != nil {
		c.readHandler.Close()
	}
	if c.writeHandler != nil {
		c.writeHandler.Close()
	}

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

// reconnectIfNeeded checks for connection errors and reconnects
// Returns true if reconnection was successful
func (c *Client) reconnectIfNeeded(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	if strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") {
		slog.Warn("insight connection lost, reconnecting", "error", err)
		if reconnErr := c.connectLocked(); reconnErr != nil {
			slog.Error("insight reconnect failed", "error", reconnErr)
			return false
		}
		slog.Info("insight reconnected")
		return true
	}
	return false
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
func (c *Client) ReadRegister(unitID byte, register uint16) (uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		c.readHandler.SlaveId = unitID
		data, err := c.readClient.ReadHoldingRegisters(register, 1)
		if err != nil {
			c.lastErr = err
			if c.reconnectIfNeeded(err) && attempt == 0 {
				continue
			}
			return 0, fmt.Errorf("read register %d from unit %d: %w", register, unitID, err)
		}

		if len(data) < 2 {
			return 0, fmt.Errorf("short response from unit %d", unitID)
		}

		return uint16(data[0])<<8 | uint16(data[1]), nil
	}
	return 0, fmt.Errorf("read register failed after retry")
}

// ReadHoldingRegisters reads multiple consecutive holding registers
func (c *Client) ReadHoldingRegisters(unitID byte, startRegister uint16, count uint16) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		c.readHandler.SlaveId = unitID
		data, err := c.readClient.ReadHoldingRegisters(startRegister, count)
		if err != nil {
			c.lastErr = err
			if c.reconnectIfNeeded(err) && attempt == 0 {
				continue
			}
			return nil, fmt.Errorf("read holding registers %d-%d from unit %d: %w", startRegister, startRegister+count-1, unitID, err)
		}

		expected := int(count * 2)
		if len(data) < expected {
			return nil, fmt.Errorf("short response from unit %d: got %d bytes, expected %d", unitID, len(data), expected)
		}

		result := make([]uint16, count)
		for i := uint16(0); i < count; i++ {
			result[i] = uint16(data[i*2])<<8 | uint16(data[i*2+1])
		}
		return result, nil
	}
	return nil, fmt.Errorf("read holding registers failed after retry")
}

// WriteRegister writes a single holding register to an inverter
func (c *Client) WriteRegister(unitID byte, register uint16, value uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Try up to 2 times (initial + 1 retry after reconnect)
	for attempt := 0; attempt < 2; attempt++ {
		c.waitForGap()

		c.writeHandler.SlaveId = unitID
		_, err := c.writeClient.WriteSingleRegister(register, value)
		c.lastWriteAt = time.Now()

		if err != nil {
			c.lastErr = err
			if c.reconnectIfNeeded(err) && attempt == 0 {
				// Reconnected, retry once
				continue
			}
			return fmt.Errorf("write register %d = %d to unit %d: %w", register, value, unitID, err)
		}
		return nil
	}
	return fmt.Errorf("write register failed after retry")
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

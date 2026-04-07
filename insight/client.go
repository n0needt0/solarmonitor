package insight

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goburrow/modbus"
)

// operationTimeout is the hard deadline for any single Modbus call.
// If the underlying TCP Read/Write hangs beyond this, the operation
// is abandoned and the connection is replaced on the next call.
const operationTimeout = 10 * time.Second

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
	stuck       atomic.Bool // set when an operation timed out; forces reconnect on next call
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
	if err := c.connectReadLocked(); err != nil {
		return err
	}
	if err := c.connectWriteLocked(); err != nil {
		c.readHandler.Close()
		return err
	}
	return nil
}

// connectReadLocked reconnects read handler only (must hold lock)
func (c *Client) connectReadLocked() error {
	if c.readHandler != nil {
		c.readHandler.Close()
	}
	c.readHandler = modbus.NewTCPClientHandler(fmt.Sprintf("%s:%d", c.host, c.readPort))
	c.readHandler.Timeout = time.Duration(c.timeoutMs) * time.Millisecond
	if err := c.readHandler.Connect(); err != nil {
		return fmt.Errorf("connect read port %d: %w", c.readPort, err)
	}
	c.readClient = modbus.NewClient(c.readHandler)
	return nil
}

// connectWriteLocked reconnects write handler only (must hold lock)
func (c *Client) connectWriteLocked() error {
	if c.writeHandler != nil {
		c.writeHandler.Close()
	}
	c.writeHandler = modbus.NewTCPClientHandler(fmt.Sprintf("%s:%d", c.host, c.writePort))
	c.writeHandler.Timeout = time.Duration(c.timeoutMs) * time.Millisecond
	if err := c.writeHandler.Connect(); err != nil {
		return fmt.Errorf("connect write port %d: %w", c.writePort, err)
	}
	c.writeClient = modbus.NewClient(c.writeHandler)
	return nil
}

// isConnectionError returns true if err indicates a dropped or stale connection
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "no route to host") ||
		strings.Contains(errStr, "operation timed out") ||
		strings.Contains(errStr, "does not match request")
}

// IsModbusException3 returns true if err is a Modbus exception 3 (illegal data value).
// This typically indicates the gateway lost Xanbus communication to the inverters.
func IsModbusException3(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "exception '3'")
}

// reconnectRead reconnects only the read handler. Returns true if successful.
func (c *Client) reconnectRead(err error) bool {
	if !isConnectionError(err) {
		return false
	}
	slog.Debug("read connection lost, reconnecting", "error", err)
	if reconnErr := c.connectReadLocked(); reconnErr != nil {
		slog.Error("read reconnect failed", "error", reconnErr)
		return false
	}
	slog.Debug("read connection restored")
	return true
}

// reconnectWrite reconnects only the write handler. Returns true if successful.
func (c *Client) reconnectWrite(err error) bool {
	if !isConnectionError(err) {
		return false
	}
	slog.Debug("write connection lost, reconnecting", "error", err)
	if reconnErr := c.connectWriteLocked(); reconnErr != nil {
		slog.Error("write reconnect failed", "error", reconnErr)
		return false
	}
	slog.Debug("write connection restored")
	return true
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

// recoverIfStuck recreates both connections if a previous operation timed out.
// Must hold c.mu.
func (c *Client) recoverIfStuck() {
	if !c.stuck.Load() {
		return
	}
	slog.Warn("recovering from stuck operation — recreating connections")
	// Don't Close old handlers — they're leaked (stuck goroutine still holds mb.mu).
	// Just nil them out so connectXxxLocked creates fresh ones.
	c.readHandler = nil
	c.readClient = nil
	c.writeHandler = nil
	c.writeClient = nil
	c.stuck.Store(false)

	if err := c.connectLocked(); err != nil {
		slog.Error("recovery reconnect failed", "error", err)
	}
}

// ReadRegister reads a single holding register from an inverter
func (c *Client) ReadRegister(unitID byte, register uint16) (uint16, error) {
	type result struct {
		val uint16
		err error
	}

	ch := make(chan result, 1)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.recoverIfStuck()

		for attempt := 0; attempt < 2; attempt++ {
			c.readHandler.SlaveId = unitID
			data, err := c.readClient.ReadHoldingRegisters(register, 1)
			if err != nil {
				if c.reconnectRead(err) && attempt == 0 {
					continue
				}
				ch <- result{0, fmt.Errorf("read register %d from unit %d: %w", register, unitID, err)}
				return
			}

			if len(data) < 2 {
				ch <- result{0, fmt.Errorf("short response from unit %d", unitID)}
				return
			}

			ch <- result{uint16(data[0])<<8 | uint16(data[1]), nil}
			return
		}
		ch <- result{0, fmt.Errorf("read register failed after retry")}
	}()

	select {
	case r := <-ch:
		return r.val, r.err
	case <-time.After(operationTimeout):
		slog.Error("ReadRegister timeout — operation stuck, will reconnect on next call",
			"unit", unitID, "register", register)
		// Mark as stuck. The goroutine still holds c.mu but will eventually return
		// when the TCP retransmit gives up. Next caller will wait for mu then recover.
		c.stuck.Store(true)
		return 0, fmt.Errorf("read register %d from unit %d: operation timeout (%v)", register, unitID, operationTimeout)
	}
}

// ReadHoldingRegisters reads multiple consecutive holding registers
func (c *Client) ReadHoldingRegisters(unitID byte, startRegister uint16, count uint16) ([]uint16, error) {
	type result struct {
		data []uint16
		err  error
	}

	ch := make(chan result, 1)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.recoverIfStuck()

		for attempt := 0; attempt < 2; attempt++ {
			c.readHandler.SlaveId = unitID
			data, err := c.readClient.ReadHoldingRegisters(startRegister, count)
			if err != nil {
				if c.reconnectRead(err) && attempt == 0 {
					continue
				}
				ch <- result{nil, fmt.Errorf("read holding registers %d-%d from unit %d: %w", startRegister, startRegister+count-1, unitID, err)}
				return
			}

			expected := int(count * 2)
			if len(data) < expected {
				ch <- result{nil, fmt.Errorf("short response from unit %d: got %d bytes, expected %d", unitID, len(data), expected)}
				return
			}

			vals := make([]uint16, count)
			for i := uint16(0); i < count; i++ {
				vals[i] = uint16(data[i*2])<<8 | uint16(data[i*2+1])
			}
			ch <- result{vals, nil}
			return
		}
		ch <- result{nil, fmt.Errorf("read holding registers failed after retry")}
	}()

	select {
	case r := <-ch:
		return r.data, r.err
	case <-time.After(operationTimeout):
		slog.Error("ReadHoldingRegisters timeout — operation stuck, will reconnect on next call",
			"unit", unitID, "start", startRegister, "count", count)
		c.stuck.Store(true)
		return nil, fmt.Errorf("read holding registers %d-%d from unit %d: operation timeout (%v)", startRegister, startRegister+count-1, unitID, operationTimeout)
	}
}

// conditionBus sends a throwaway read on the read port to wake up the
// gateway's Modbus TCP-to-XanBus bridge for a specific unit. Called before
// writes when the bus has been idle, since the first device in the chain
// (unit 10) tends to reject writes after long quiet periods.
func (c *Client) conditionBus(unitID byte) {
	c.readHandler.SlaveId = unitID
	_, _ = c.readClient.ReadHoldingRegisters(RegEPCModeCommand, 1)
}

// busIdleThreshold is how long the write port must be idle before we send
// a conditioning read to wake up the gateway bridge.
const busIdleThreshold = 30 * time.Second

// WriteRegister writes a single holding register to an inverter
func (c *Client) WriteRegister(unitID byte, register uint16, value uint16) error {
	ch := make(chan error, 1)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.recoverIfStuck()

		// Wake up the gateway bridge if the bus has been quiet
		if !c.lastWriteAt.IsZero() && time.Since(c.lastWriteAt) > busIdleThreshold {
			c.conditionBus(unitID)
		}

		for attempt := 0; attempt < 2; attempt++ {
			c.waitForGap()

			c.writeHandler.SlaveId = unitID
			_, err := c.writeClient.WriteSingleRegister(register, value)
			c.lastWriteAt = time.Now()

			if err != nil {
				if c.reconnectWrite(err) && attempt == 0 {
					continue
				}
				ch <- fmt.Errorf("write register %d = %d to unit %d: %w", register, value, unitID, err)
				return
			}
			ch <- nil
			return
		}
		ch <- fmt.Errorf("write register failed after retry")
	}()

	select {
	case err := <-ch:
		return err
	case <-time.After(operationTimeout):
		slog.Error("WriteRegister timeout — operation stuck, will reconnect on next call",
			"unit", unitID, "register", register, "value", value)
		c.stuck.Store(true)
		return fmt.Errorf("write register %d = %d to unit %d: operation timeout (%v)", register, value, unitID, operationTimeout)
	}
}

package roborock

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	miioPort    = 54321
	miioUDPNet  = "udp4"
	readBufSize = 4096
)

// Client controls a single Roborock vacuum over the local miIO UDP protocol.
type Client struct {
	name    string
	host    string
	token   []byte
	keys    keys
	timeout time.Duration

	mu       sync.Mutex
	conn     *net.UDPConn
	deviceID uint32
	stamp    uint32
	handshook bool

	msgID atomic.Uint32
}

// NewClient creates a Roborock client.
// token is a 32-char hex string (e.g. "a1b2c3d4e5f6...").
func NewClient(name, host, tokenHex string, timeout time.Duration) (*Client, error) {
	raw, err := hex.DecodeString(tokenHex)
	if err != nil {
		return nil, fmt.Errorf("roborock %s: invalid token (must be 32-char hex): %w", name, err)
	}
	if len(raw) != 16 {
		return nil, fmt.Errorf("roborock %s: token must decode to 16 bytes, got %d", name, len(raw))
	}
	return &Client{
		name:    name,
		host:    host,
		token:   raw,
		keys:    deriveKeys(raw),
		timeout: timeout,
	}, nil
}

// Name returns the human-readable vacuum label.
func (c *Client) Name() string { return c.name }

// Host returns the vacuum's IP address.
func (c *Client) Host() string { return c.host }

// Handshake sends the hello packet and captures the device ID and stamp.
// Called automatically by call() if not yet done, and on checksum errors.
func (c *Client) Handshake(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handshakeLocked(ctx)
}

func (c *Client) handshakeLocked(ctx context.Context) error {
	conn, err := c.dialLocked()
	if err != nil {
		return err
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(c.timeout)
	}
	conn.SetDeadline(deadline)

	if _, err := conn.Write(helloPacket()); err != nil {
		return fmt.Errorf("handshake write: %w", err)
	}

	buf := make([]byte, readBufSize)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("handshake read: %w", err)
	}

	// Hello response has no payload; use empty keys for decode.
	f, err := decode(buf[:n], keys{key: make([]byte, 16), iv: make([]byte, 16)})
	if err != nil {
		return fmt.Errorf("handshake decode: %w", err)
	}

	c.deviceID = f.deviceID
	c.stamp = f.stamp
	c.handshook = true
	slog.Debug("roborock: handshake ok", "name", c.name, "deviceID", c.deviceID, "stamp", c.stamp)
	return nil
}

func (c *Client) dialLocked() (*net.UDPConn, error) {
	if c.conn != nil {
		return c.conn, nil
	}
	addr, err := net.ResolveUDPAddr(miioUDPNet, fmt.Sprintf("%s:%d", c.host, miioPort))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP(miioUDPNet, nil, addr)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

// Status fetches the current vacuum status.
func (c *Client) Status(ctx context.Context) (VacuumStatus, error) {
	raw, err := c.call(ctx, "get_status", nil)
	if err != nil {
		return VacuumStatus{}, err
	}
	// get_status returns an array with one object.
	var results []VacuumStatus
	if err := json.Unmarshal(raw, &results); err != nil {
		return VacuumStatus{}, fmt.Errorf("get_status decode: %w", err)
	}
	if len(results) == 0 {
		return VacuumStatus{}, fmt.Errorf("get_status: empty result")
	}
	return results[0], nil
}

// CleanSummary fetches the cleaning history.
func (c *Client) CleanSummary(ctx context.Context) (CleanSummary, error) {
	raw, err := c.call(ctx, "get_clean_summary", nil)
	if err != nil {
		return CleanSummary{}, err
	}
	return UnmarshalCleanSummary(raw)
}

// StartOrResume starts a new clean or resumes a paused one.
// If paused is true, app_resume is tried first (some models require it),
// falling back to app_start if the device returns an error.
func (c *Client) StartOrResume(ctx context.Context, paused bool) error {
	if paused {
		if err := c.command(ctx, "app_resume"); err == nil {
			slog.Info("roborock: resumed", "name", c.name)
			return nil
		}
		// Fallback: app_start handles resume on some models.
		slog.Debug("roborock: app_resume failed, trying app_start", "name", c.name)
	}
	if err := c.command(ctx, "app_start"); err != nil {
		return err
	}
	slog.Info("roborock: started", "name", c.name)
	return nil
}

// Pause pauses the current cleaning cycle.
func (c *Client) Pause(ctx context.Context) error {
	if err := c.command(ctx, "app_pause"); err != nil {
		return err
	}
	slog.Info("roborock: paused", "name", c.name)
	return nil
}

// Charge sends the vacuum back to its dock.
func (c *Client) Charge(ctx context.Context) error {
	if err := c.command(ctx, "app_charge"); err != nil {
		return err
	}
	slog.Info("roborock: returning to dock", "name", c.name)
	return nil
}

// command calls a method with no params and ignores the result.
func (c *Client) command(ctx context.Context, method string) error {
	_, err := c.call(ctx, method, []any{})
	return err
}

// call sends a miIO JSON-RPC request and returns the raw result field.
// It handles handshake on first call and re-handshakes on checksum errors.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.handshook {
		if err := c.handshakeLocked(ctx); err != nil {
			return nil, fmt.Errorf("roborock %s: handshake: %w", c.name, err)
		}
	}

	result, err := c.callLocked(ctx, method, params)
	if err != nil {
		// On checksum error or timeout, re-handshake and retry once.
		slog.Debug("roborock: call failed, re-handshaking", "name", c.name, "err", err)
		c.handshook = false
		if hsErr := c.handshakeLocked(ctx); hsErr != nil {
			return nil, fmt.Errorf("roborock %s re-handshake: %w", c.name, hsErr)
		}
		result, err = c.callLocked(ctx, method, params)
	}
	return result, err
}

type rpcRequest struct {
	ID     uint32 `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcResponse struct {
	ID     uint32          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.msgID.Add(1)

	payload, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}

	pkt, err := encode(frame{
		deviceID: c.deviceID,
		stamp:    c.stamp,
		payload:  payload,
	}, c.keys)
	if err != nil {
		return nil, err
	}

	conn, err := c.dialLocked()
	if err != nil {
		return nil, err
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(c.timeout)
	}
	conn.SetDeadline(deadline)

	if _, err := conn.Write(pkt); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	buf := make([]byte, readBufSize)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}

	resp, err := decode(buf[:n], c.keys)
	if err != nil {
		return nil, err
	}
	// Update stamp from device's response for next call.
	c.stamp = resp.stamp

	var rpc rpcResponse
	if err := json.Unmarshal(resp.payload, &rpc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil
}

// Close releases the UDP connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

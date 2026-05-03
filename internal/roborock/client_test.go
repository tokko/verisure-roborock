package roborock

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestCloseConnLocked verifies that closeConnLocked nils c.conn and resets handshook.
func TestCloseConnLocked(t *testing.T) {
	c := &Client{
		name:    "test",
		host:    "127.0.0.1",
		timeout: time.Second,
	}

	// Initially no connection.
	if c.conn != nil {
		t.Fatal("expected nil conn initially")
	}

	// Dialing creates a connection.
	conn, err := c.dialLocked()
	if err != nil {
		t.Fatalf("dialLocked: %v", err)
	}
	if conn == nil || c.conn == nil {
		t.Fatal("expected non-nil conn after dialLocked")
	}

	// Mark as handshook to confirm reset.
	c.handshook = true

	// closeConnLocked must nil the connection and reset handshook.
	c.closeConnLocked()
	if c.conn != nil {
		t.Error("expected c.conn = nil after closeConnLocked")
	}
	if c.handshook {
		t.Error("expected handshook = false after closeConnLocked")
	}
}

// TestCloseConnLockedIdempotent verifies double-close doesn't panic.
func TestCloseConnLockedIdempotent(t *testing.T) {
	c := &Client{name: "test", host: "127.0.0.1", timeout: time.Second}
	// Safe to call on nil conn.
	c.closeConnLocked()
	c.closeConnLocked()
	if c.conn != nil {
		t.Error("expected conn to remain nil")
	}
}

// TestDialLockedCreatesNewConnAfterClose verifies that after closeConnLocked,
// dialLocked creates a fresh socket (different object, new ephemeral port).
func TestDialLockedCreatesNewConnAfterClose(t *testing.T) {
	c := &Client{
		name:    "test",
		host:    "127.0.0.1",
		timeout: time.Second,
	}

	conn1, err := c.dialLocked()
	if err != nil {
		t.Fatalf("first dialLocked: %v", err)
	}
	ptr1 := conn1 // pointer identity

	c.closeConnLocked()

	conn2, err := c.dialLocked()
	if err != nil {
		t.Fatalf("second dialLocked: %v", err)
	}

	if ptr1 == conn2 {
		t.Error("dialLocked returned the same connection object after closeConnLocked; expected a fresh socket")
	}

	// Cleanup.
	conn2.Close()
	c.conn = nil
}

// TestHandshakeFailureClosesConn verifies that a failed handshake (no server reply)
// leaves c.conn == nil so the next attempt creates a fresh socket.
//
// It uses a real UDP socket that receives but never replies, and a very short timeout
// so the test runs quickly.
func TestHandshakeFailureClosesConn(t *testing.T) {
	// Start a sink server: accept packets but never reply.
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	srvAddr := srv.LocalAddr().(*net.UDPAddr)

	c := &Client{
		name:    "test",
		host:    srvAddr.IP.String(),
		timeout: 50 * time.Millisecond, // short timeout so test is fast
	}

	// Manually set the port (NewClient doesn't let us override it).
	// We bypass NewClient since we don't need a real token for this test.
	c.keys = deriveKeys(make([]byte, 16))

	// Override the host to include port — but dialLocked always appends miioPort.
	// Instead we patch the addr resolution by pointing host to a loopback address
	// and using DialUDP directly.  The simplest path is to pre-dial the socket
	// ourselves to the test server and inject it.
	addr := &net.UDPAddr{IP: srvAddr.IP, Port: srvAddr.Port}
	injectedConn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		t.Fatalf("pre-dial: %v", err)
	}
	c.conn = injectedConn

	// Handshake will time out because srv never replies.
	ctx := context.Background()
	err = c.handshakeLocked(ctx)
	if err == nil {
		t.Fatal("expected handshakeLocked to fail (no server reply)")
	}

	// The key assertion: conn must be nil after the failure.
	if c.conn != nil {
		t.Error("expected c.conn = nil after handshake failure, but it was still set")
		c.conn.Close()
		c.conn = nil
	}
	if c.handshook {
		t.Error("expected handshook = false after handshake failure")
	}
}

// TestCallLockedRecvFailureClosesConn verifies that a recv error in callLocked
// leaves c.conn == nil so the next attempt gets a fresh socket.
func TestCallLockedRecvFailureClosesConn(t *testing.T) {
	// Sink server: receive but never reply.
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	srvAddr := srv.LocalAddr().(*net.UDPAddr)

	c := &Client{
		name:    "test",
		host:    srvAddr.IP.String(),
		timeout: 50 * time.Millisecond,
	}
	c.keys = deriveKeys(make([]byte, 16))

	// Inject pre-dialled connection.
	addr := &net.UDPAddr{IP: srvAddr.IP, Port: srvAddr.Port}
	injectedConn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		t.Fatalf("pre-dial: %v", err)
	}
	c.conn = injectedConn
	c.handshook = true // pretend we already handshook
	c.stamp = 42

	ctx := context.Background()
	_, err = c.callLocked(ctx, "get_status", nil)
	if err == nil {
		t.Fatal("expected callLocked to fail (no server reply)")
	}

	if c.conn != nil {
		t.Error("expected c.conn = nil after recv failure, but it was still set")
		c.conn.Close()
		c.conn = nil
	}
}

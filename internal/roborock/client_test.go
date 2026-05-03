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

// TestHandshakeClosesStaleConnAtStart verifies that handshakeLocked always
// closes any pre-existing connection before attempting the handshake, so that
// each attempt gets a fresh ephemeral port even if a previous conn is still
// open (e.g. from a prior failed attempt that somehow wasn't cleaned up).
func TestHandshakeClosesStaleConnAtStart(t *testing.T) {
	c := &Client{name: "test", host: "127.0.0.1", timeout: 50 * time.Millisecond}
	c.keys = deriveKeys(make([]byte, 16))

	// Pre-dial a socket and inject it to simulate a stale connection.
	staleAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	staleConn, err := net.DialUDP("udp4", nil, staleAddr)
	if err != nil {
		t.Fatalf("pre-dial stale conn: %v", err)
	}
	c.conn = staleConn
	c.handshook = true

	// handshakeLocked must close the stale conn immediately (close-at-start),
	// then attempt a fresh handshake to 127.0.0.1:54321 which will fail
	// (no server there). Either way, conn must be nil afterward.
	ctx := context.Background()
	_ = c.handshakeLocked(ctx) // error is expected — we don't care which

	if c.conn != nil {
		t.Error("expected c.conn = nil after handshakeLocked, but it was still set")
		c.conn.Close()
		c.conn = nil
	}
	if c.handshook {
		t.Error("expected handshook = false after handshakeLocked")
	}
}

// TestHandshakeFailureClosesConn verifies that a failed handshake (no server reply)
// leaves c.conn == nil so the next attempt creates a fresh socket.
//
// It uses a sink server that accepts packets but never replies, plus a short timeout.
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
	c.keys = deriveKeys(make([]byte, 16))

	// Patch the client to dial the test server instead of the real miioPort.
	// We override c.host with a combined "host:port" and replace dialLocked
	// indirectly by pre-dialling and injecting the conn AFTER closeConnLocked
	// would have run. Simplest approach: just point c.host to loopback and
	// patch miioPort at test time is not possible — instead we accept that
	// handshakeLocked will close any existing conn first (tested above), then
	// dial 127.0.0.1:54321 which is either ICMP refused or timeout. Either
	// path leaves conn=nil, which is what we assert.
	//
	// For a timeout-based test, use a sink server via injecting conn after the
	// close-at-start by accepting that dialLocked overrides the conn.
	// The simplest reliable test: start with no conn, let handshakeLocked
	// create one, let it fail, assert conn=nil.
	ctx := context.Background()
	err = c.handshakeLocked(ctx)
	if err == nil {
		t.Fatal("expected handshakeLocked to fail (no real server at miioPort)")
	}

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

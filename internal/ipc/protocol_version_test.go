package ipc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// (a) A matched pair: a Dial'd Client against a current Server reaches a
// gated method's handler normally, and health reports the daemon's version.
func TestMatchedProtocolVersionReachesHandlerAndHealthReportsVersion(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	var handlerRan atomic.Bool
	srv.Handle("echo", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		handlerRan.Store(true)
		return map[string]bool{"ok": true}, nil
	})
	// A version this binary never speaks, so the assertion below can only pass
	// by carrying the answering daemon's own value rather than the constant
	// the client already knows.
	const daemonReportedVersion = ipc.ProtocolVersion + 41
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: daemonReportedVersion}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var raw json.RawMessage
	if err := c.Call("echo", nil, &raw); err != nil {
		t.Fatalf("echo call: %v", err)
	}
	if !handlerRan.Load() {
		t.Fatal("expected matched-version request to reach the handler")
	}

	var health ipc.HealthResult
	if err := c.Call(ipc.MethodHealth, nil, &health); err != nil {
		t.Fatalf("health call: %v", err)
	}
	if health.ProtocolVersion != daemonReportedVersion {
		t.Errorf("health.ProtocolVersion = %d, want the daemon's reported %d", health.ProtocolVersion, daemonReportedVersion)
	}
}

// writeRawRequestLine writes a raw JSON-RPC request line with no
// protocol_version field, simulating an old client.
func writeRawRequestLine(t *testing.T, conn interface{ Write([]byte) (int, error) }, id int64, method string) {
	t.Helper()
	writeRawRequestLineAtVersion(t, conn, id, method, 0)
}

// writeRawRequestLineAtVersion writes a raw JSON-RPC request line stamped with
// an explicit protocol version. Version 0 omits the field, which is exactly
// what a pre-handshake client sends.
func writeRawRequestLineAtVersion(t *testing.T, conn interface{ Write([]byte) (int, error) }, id int64, method string, version int) {
	t.Helper()
	req := struct {
		JSONRPC         string          `json:"jsonrpc"`
		Method          string          `json:"method"`
		Params          json.RawMessage `json:"params,omitempty"`
		ID              int64           `json:"id"`
		ProtocolVersion int             `json:"protocol_version,omitempty"`
	}{JSONRPC: "2.0", Method: method, ID: id, ProtocolVersion: version}
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal raw request: %v", err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("write raw request: %v", err)
	}
}

// assertConnectionStillServesHealth proves a refused connection is still
// live: the health handler is registered first, so a successful decode with
// the daemon's version can only come from the handler actually running.
func assertConnectionStillServesHealth(t *testing.T, srv *ipc.Server, conn interface {
	Write([]byte) (int, error)
}, scanner *bufio.Scanner) {
	t.Helper()
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: ipc.ProtocolVersion}, nil
	})
	writeRawRequestLine(t, conn, 2, ipc.MethodHealth)
	if !scanner.Scan() {
		t.Fatalf("expected a follow-up response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal follow-up response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("follow-up health call failed: %+v", resp.Error)
	}
	var health ipc.HealthResult
	if err := json.Unmarshal(resp.Result, &health); err != nil {
		t.Fatalf("unmarshal follow-up result: %v", err)
	}
	if health.Status != "ok" || health.ProtocolVersion != ipc.ProtocolVersion {
		t.Fatalf("follow-up health = %+v, want status ok and version %d", health, ipc.ProtocolVersion)
	}
}

// (b) An old client (no protocol_version) calling a gated method is refused
// with ErrProtocolMismatch, the handler never runs, and the connection stays
// usable afterward.
func TestOldClientGatedMethodFailsClosedWithoutRunningHandler(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	var handlerRan atomic.Bool
	srv.Handle("gated", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		handlerRan.Store(true)
		return map[string]bool{"ok": true}, nil
	})

	conn := rawDial(t, sock)
	defer conn.Close()

	writeRawRequestLine(t, conn, 1, "gated")

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected a response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != ipc.ErrProtocolMismatch {
		t.Errorf("code = %d, want %d", resp.Error.Code, ipc.ErrProtocolMismatch)
	}
	wantMsg := (&ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleClient}).Error()
	if resp.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", resp.Error.Message, wantMsg)
	}
	if handlerRan.Load() {
		t.Fatal("expected the handler to never run for a mismatched-version request")
	}

	assertConnectionStillServesHealth(t, srv, conn, scanner)
}

// (b, stream variant) An old client subscribing to a stream method is
// refused the same way: no stream starts, and the connection stays usable.
func TestOldClientStreamMethodFailsClosedWithoutStartingStream(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	var streamStarted atomic.Bool
	srv.HandleStream(ipc.MethodSubscribe, func(_ context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
		streamStarted.Store(true)
		return func(send func(interface{}) error) error {
			return nil
		}, nil
	})

	conn := rawDial(t, sock)
	defer conn.Close()

	writeRawRequestLine(t, conn, 1, ipc.MethodSubscribe)

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected a response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != ipc.ErrProtocolMismatch {
		t.Errorf("code = %d, want %d", resp.Error.Code, ipc.ErrProtocolMismatch)
	}
	wantMsg := (&ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleClient}).Error()
	if resp.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", resp.Error.Message, wantMsg)
	}
	if streamStarted.Load() {
		t.Fatal("expected no stream to start for a mismatched-version request")
	}

	assertConnectionStillServesHealth(t, srv, conn, scanner)
}

// (c) An old client's health call always succeeds, and reports the daemon's
// version, even though the request carries no protocol_version.
func TestOldClientHealthCallSucceedsAndReportsDaemonVersion(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)
	// Distinguishable from this binary's own version, so the assertion below
	// proves the daemon's value travelled rather than that an int survives JSON.
	const daemonReportedVersion = ipc.ProtocolVersion + 41
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: daemonReportedVersion}, nil
	})

	conn := rawDial(t, sock)
	defer conn.Close()

	writeRawRequestLine(t, conn, 1, ipc.MethodHealth)

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected a response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	var health ipc.HealthResult
	if err := json.Unmarshal(resp.Result, &health); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if health.ProtocolVersion != daemonReportedVersion {
		t.Errorf("health.ProtocolVersion = %d, want the daemon's reported %d", health.ProtocolVersion, daemonReportedVersion)
	}
}

// Skew is not only "peer predates the handshake". The day ProtocolVersion is
// bumped past 1, the live direction is a NEWER peer, and the remedy must name
// the stale side rather than whichever side noticed.
func TestNewerClientGatedMethodIsRefusedAndBlamesTheStaleDaemon(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	var handlerRan atomic.Bool
	srv.Handle("gated", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		handlerRan.Store(true)
		return map[string]bool{"ok": true}, nil
	})

	conn := rawDial(t, sock)
	defer conn.Close()

	const newerClientVersion = ipc.ProtocolVersion + 1
	writeRawRequestLineAtVersion(t, conn, 1, "gated", newerClientVersion)

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected a response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != ipc.ErrProtocolMismatch {
		t.Fatalf("a newer peer must be refused too, got %+v", resp.Error)
	}
	if handlerRan.Load() {
		t.Fatal("expected the handler to never run for a newer-version request")
	}
	// This server is the stale side of the pairing, so the message must send
	// the user at the daemon, not at their own binary.
	if !strings.Contains(resp.Error.Message, "Run 'no-mistakes daemon restart'") {
		t.Errorf("expected the remedy to name the stale daemon, got %q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, fmt.Sprintf("client speaks version %d", newerClientVersion)) {
		t.Errorf("expected the client's own version to be reported, got %q", resp.Error.Message)
	}

	assertConnectionStillServesHealth(t, srv, conn, scanner)
}

// Shutdown is the remedy the mismatch message prescribes, so a skewed peer
// must be able to reach it; otherwise a stale CLI cannot replace the daemon
// it is told to restart.
func TestOldClientShutdownReachesHandlerDespiteVersionSkew(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	var handlerRan atomic.Bool
	srv.Handle(ipc.MethodShutdown, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		handlerRan.Store(true)
		return map[string]bool{"ok": true}, nil
	})

	conn := rawDial(t, sock)
	defer conn.Close()

	writeRawRequestLine(t, conn, 1, ipc.MethodShutdown)

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected a response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("shutdown must not be refused for a skewed peer, got: %+v", resp.Error)
	}
	if !handlerRan.Load() {
		t.Fatal("expected the shutdown handler to run for a mismatched-version request")
	}
}

package ipc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// gate_context is the containment query every pipeline-control command runs
// before it does anything. Gating it deadlocks the whole remedy: `daemon
// stop`, `daemon restart` and `update` all refuse with the very error that
// tells the user to run them. It must answer a skewed peer, so the
// authenticated peer-ancestry verdict stays available across skew.
func TestGateContextReachesHandlerForSkewedPeer(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	var handlerRan atomic.Bool
	srv.Handle(ipc.MethodGateContext, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		handlerRan.Store(true)
		return ipc.GateContextResult{Nested: true, RunID: "run-1"}, nil
	})

	conn := rawDial(t, sock)
	defer conn.Close()

	writeRawRequestLine(t, conn, 1, ipc.MethodGateContext)

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected a response, scan err: %v", scanner.Err())
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("gate_context must answer a skewed peer, got error: %+v", resp.Error)
	}
	if !handlerRan.Load() {
		t.Fatal("expected the gate_context handler to run for a version-skewed peer")
	}

	var result ipc.GateContextResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !result.Nested || result.RunID != "run-1" {
		t.Errorf("skewed peer got a degraded verdict: %+v", result)
	}
}

// The exemption is exactly three meta methods; every method that carries
// pipeline payload stays gated.
func TestOnlyMetaMethodsAreExemptFromTheVersionGate(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	gated := []string{
		ipc.MethodGetRun, ipc.MethodRespond, ipc.MethodCancelRun,
		// The hook methods carry the containment verdict the pre-receive gate
		// enforces, so a skewed peer must never reach them either.
		ipc.MethodAdmitPush, ipc.MethodPushReceived,
		// Streaming dispatch takes a separate path through handleConn, so the
		// gate has to run before the stream handler lookup too.
		ipc.MethodSubscribe,
	}
	for _, method := range gated {
		if method == ipc.MethodSubscribe {
			srv.HandleStream(method, func(_ context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
				return func(send func(interface{}) error) error {
					return send(map[string]bool{"ok": true})
				}, nil
			})
			continue
		}
		srv.Handle(method, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
			return map[string]bool{"ok": true}, nil
		})
	}

	conn := rawDial(t, sock)
	defer conn.Close()
	scanner := bufio.NewScanner(conn)

	for i, method := range gated {
		writeRawRequestLine(t, conn, int64(i+1), method)
		if !scanner.Scan() {
			t.Fatalf("%s: expected a response, scan err: %v", method, scanner.Err())
		}
		var resp ipc.Response
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("%s: unmarshal response: %v", method, err)
		}
		if resp.Error == nil || resp.Error.Code != ipc.ErrProtocolMismatch {
			t.Errorf("%s must stay gated for a skewed peer, got %+v", method, resp.Error)
		}
	}
}

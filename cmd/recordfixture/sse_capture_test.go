package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestOpencodeSSECaptureWaitsForIdle(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- capture.WaitForIdle(ctx)
	}()

	if _, err := capture.Write([]byte("event: message\ndata: {\"type\":\"session.idle\"}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("wait for idle: %v", err)
	}
	if !bytes.Contains(capture.Bytes(), []byte("\"session.idle\"")) {
		t.Fatalf("captured bytes = %q, want session.idle", capture.Bytes())
	}
}

func TestOpencodeSSECaptureIgnoresSessionIdleSubstringOutsideIdleEvent(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("")
	idleCtx, idleCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer idleCancel()

	if _, err := capture.Write([]byte("event: message\ndata: {\"type\":\"message.updated\",\"text\":\"contains \\\"session.idle\\\" in content\"}\n\n")); err != nil {
		t.Fatalf("write non-idle event: %v", err)
	}
	if err := capture.WaitForIdle(idleCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for idle error = %v, want deadline exceeded", err)
	}

	realIdleCtx, realIdleCancel := context.WithTimeout(t.Context(), time.Second)
	defer realIdleCancel()
	if _, err := capture.Write([]byte("event: message\ndata: {\"type\":\"session.idle\"}\n\n")); err != nil {
		t.Fatalf("write idle event: %v", err)
	}
	if err := capture.WaitForIdle(realIdleCtx); err != nil {
		t.Fatalf("wait for real idle: %v", err)
	}
}

func TestOpencodeSSECaptureRecognizesNestedSessionIdleEvent(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	if _, err := capture.Write([]byte("event: message\ndata: {\"payload\":{\"type\":\"session.idle\"}}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := capture.WaitForIdle(ctx); err != nil {
		t.Fatalf("wait for nested idle: %v", err)
	}
}

func TestOpencodeSSECaptureRecognizesTopLevelSessionIdleEvent(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("target-session")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	if _, err := capture.Write([]byte("event: message\ndata: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"target-session\"}}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := capture.WaitForIdle(ctx); err != nil {
		t.Fatalf("wait for top-level idle: %v", err)
	}
}

func TestOpencodeSSECaptureIgnoresTopLevelIdleForOtherSession(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("target-session")
	idleCtx, idleCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer idleCancel()

	if _, err := capture.Write([]byte("event: message\ndata: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"other-session\"}}\n\n")); err != nil {
		t.Fatalf("write non-target idle event: %v", err)
	}
	if err := capture.WaitForIdle(idleCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for idle error = %v, want deadline exceeded", err)
	}

	realIdleCtx, realIdleCancel := context.WithTimeout(t.Context(), time.Second)
	defer realIdleCancel()
	if _, err := capture.Write([]byte("event: message\ndata: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"target-session\"}}\n\n")); err != nil {
		t.Fatalf("write target idle event: %v", err)
	}
	if err := capture.WaitForIdle(realIdleCtx); err != nil {
		t.Fatalf("wait for target idle: %v", err)
	}
}

func TestOpencodeSSECaptureIgnoresIdleForOtherSession(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("target-session")
	idleCtx, idleCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer idleCancel()

	if _, err := capture.Write([]byte("event: message\ndata: {\"payload\":{\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"other-session\"}}}\n\n")); err != nil {
		t.Fatalf("write non-target idle event: %v", err)
	}
	if err := capture.WaitForIdle(idleCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for idle error = %v, want deadline exceeded", err)
	}

	realIdleCtx, realIdleCancel := context.WithTimeout(t.Context(), time.Second)
	defer realIdleCancel()
	if _, err := capture.Write([]byte("event: message\ndata: {\"payload\":{\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"target-session\"}}}\n\n")); err != nil {
		t.Fatalf("write target idle event: %v", err)
	}
	if err := capture.WaitForIdle(realIdleCtx); err != nil {
		t.Fatalf("wait for target idle: %v", err)
	}
}

func TestOpencodeSSECaptureExcludesEventsFromOtherSessions(t *testing.T) {
	t.Helper()

	capture := newOpencodeSSECapture("target-session")

	if _, err := capture.Write([]byte("event: message\ndata: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"other-session\"}}}\n\n")); err != nil {
		t.Fatalf("write non-target event: %v", err)
	}
	if _, err := capture.Write([]byte("event: message\ndata: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"target-session\"}}}\n\n")); err != nil {
		t.Fatalf("write target event: %v", err)
	}
	if _, err := capture.Write([]byte("event: message\ndata: {\"payload\":{\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"target-session\"}}}\n\n")); err != nil {
		t.Fatalf("write target idle: %v", err)
	}

	got := string(capture.Bytes())
	if bytes.Contains([]byte(got), []byte("other-session")) {
		t.Fatalf("captured bytes = %q, want to exclude other-session events", got)
	}
	if !bytes.Contains([]byte(got), []byte("target-session")) {
		t.Fatalf("captured bytes = %q, want target-session events", got)
	}
}

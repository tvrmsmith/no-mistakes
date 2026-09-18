package ipc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/closers"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// socketPath returns a short socket path to stay within macOS 104-byte limit.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeTempRoot(t, dir) })
	return filepath.Join(dir, "s.sock")
}

func startServer(t *testing.T, sock string) *ipc.Server {
	t.Helper()
	srv := ipc.NewServer()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()

	// wait for server to be ready
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(sock)
		if err == nil {
			closers.Quiet(c)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		srv.Close()
		if err := <-errCh; err != nil {
			t.Errorf("server returned %v on clean close, want nil", err)
		}
	})
	return srv
}

// captureLogs redirects the default logger into a sink the test can read
// safely, and restores the previous logger when the test ends.
//
// The sink is mutex-guarded because the reader is never the only writer: a
// connection the server is still tearing down logs on its own goroutine, and
// startServer's readiness probe leaves exactly one of those in flight. A plain
// bytes.Buffer races with it.
func captureLogs(t *testing.T, level slog.Level) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// removeTempRoot deletes a test's temp root and fails the test when it cannot.
// These roots are created with os.MkdirTemp rather than t.TempDir because a
// unix socket path has a small OS limit (~104 bytes on macOS) and t.TempDir
// embeds the full test name, so nothing else cleans them up. t.TempDir fails
// the test on a removal it cannot make, and so does this.
func removeTempRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("remove temp root %s: %v", dir, err)
	}
}

// sendFrame writes one raw line to a test connection. Several of these run on
// a fake server's own goroutine, where t.Errorf is allowed and t.Fatalf is
// not, and a dropped write would surface as the client timing out on a frame
// the test believes it sent.
func sendFrame(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	if _, err := io.WriteString(conn, line); err != nil {
		reportFrameWrite(t, err, fmt.Sprintf("write frame %q", line))
	}
}

// encodeFrame sends one JSON frame on the same terms as sendFrame.
func encodeFrame(t *testing.T, enc *json.Encoder, v any) {
	t.Helper()
	if err := enc.Encode(v); err != nil {
		reportFrameWrite(t, err, "encode frame")
	}
}

// reportFrameWrite fails the test on a write the client should have read. A
// client that has already hung up is the exception: a test can end the stream
// on purpose and then the frames after it are expected to go nowhere.
//
// ENOTCONN belongs in that exception alongside EPIPE. Darwin reports a write
// to a unix socket whose peer has gone as either one depending on how far the
// kernel has torn the connection down, so a loaded machine turns the same
// hang-up into a test failure only sometimes.
func reportFrameWrite(t *testing.T, err error, op string) {
	t.Helper()
	if errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ENOTCONN) {
		return
	}
	t.Errorf("%s: %v", op, err)
}

// decodeFrame reads one JSON frame a fake server received.
func decodeFrame(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Errorf("decode frame %q: %v", data, err)
	}
}

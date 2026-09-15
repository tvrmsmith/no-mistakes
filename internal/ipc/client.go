package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/closers"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var dialNetworkWithTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).Dial(network, address)
}

// ConnectTimeoutError reports a daemon IPC connect attempt that exceeded the
// bounded client timeout.
type ConnectTimeoutError struct {
	SocketPath      string
	TimeoutDuration time.Duration
	Err             error
}

func (e *ConnectTimeoutError) Error() string {
	return fmt.Sprintf("daemon socket %s did not accept a connection within %s", e.SocketPath, e.TimeoutDuration)
}

func (e *ConnectTimeoutError) Unwrap() error { return e.Err }

func (e *ConnectTimeoutError) Timeout() bool { return true }

// IsConnectTimeout reports whether err was caused by a bounded IPC connect
// timeout.
func IsConnectTimeout(err error) bool {
	var timeoutErr *ConnectTimeoutError
	return errors.As(err, &timeoutErr)
}

// CallTimeoutError reports an IPC method whose response did not arrive before
// the caller-selected read deadline. The connection was accepted; this is a
// slow or stuck reply, not a refused dial.
type CallTimeoutError struct {
	Method          string
	TimeoutDuration time.Duration
	Err             error
}

func (e *CallTimeoutError) Error() string {
	return fmt.Sprintf("daemon %s did not reply within %s", e.Method, e.TimeoutDuration)
}

func (e *CallTimeoutError) Unwrap() error { return e.Err }

func (e *CallTimeoutError) Timeout() bool { return true }

// IsCallTimeout reports whether err was caused by a bounded IPC call read
// deadline. Connect timeouts are a different failure and do not match.
func IsCallTimeout(err error) bool {
	var timeoutErr *CallTimeoutError
	return errors.As(err, &timeoutErr)
}

func isReadTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func connectTimeout() time.Duration {
	value := os.Getenv("NM_DAEMON_CONNECT_TIMEOUT")
	if value == "" {
		p, err := paths.New()
		if err != nil {
			return config.DefaultDaemonConnectTimeout
		}
		cfg, err := config.LoadGlobal(p.ConfigFile())
		if err != nil {
			return config.DefaultDaemonConnectTimeout
		}
		return cfg.DaemonConnectTimeout
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return config.DefaultDaemonConnectTimeout
	}
	return d
}

// Client connects to the IPC server over the platform transport.
type Client struct {
	conn    net.Conn
	encoder *json.Encoder
	scanner *bufio.Scanner
	mu      sync.Mutex // serializes calls on a single connection
}

const (
	// DefaultDialTimeout is the read deadline used for the daemon health check
	// dial made by callers outside this package (see internal/daemon/selfexec.go).
	DefaultDialTimeout = 250 * time.Millisecond
	// DefaultCallTimeout bounds any Call that does not name its own timeout.
	// Callers whose request can legitimately outlast it (a drain the operator
	// gave a long deadline) must use CallWithTimeout.
	DefaultCallTimeout = 30 * time.Second
)

// Dial connects to the IPC server at the given endpoint path.
func Dial(socketPath string) (*Client, error) {
	conn, err := dialEndpoint(socketPath)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	return &Client{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		scanner: scanner,
	}, nil
}

func dialEndpoint(socketPath string) (net.Conn, error) {
	timeout := connectTimeout()
	conn, err := dial(socketPath, timeout)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("dial ipc: %w", &ConnectTimeoutError{
				SocketPath:      socketPath,
				TimeoutDuration: timeout,
				Err:             err,
			})
		}
		return nil, fmt.Errorf("dial ipc: %w", err)
	}
	return conn, nil
}

// Call sends a JSON-RPC request and waits for the response.
// The result is unmarshaled into the provided pointer.
// If the server returns a JSON-RPC error, it is returned as *RPCError.
func (c *Client) Call(method string, params interface{}, result interface{}) error {
	return c.CallWithTimeout(method, params, result, DefaultCallTimeout)
}

// CallWithTimeout is Call with a caller-selected read deadline.
func (c *Client) CallWithTimeout(method string, params interface{}, result interface{}, timeout time.Duration) error {
	return c.CallWithContext(context.Background(), method, params, result, timeout)
}

// CallWithContext is CallWithTimeout with cancellation support.
func (c *Client) CallWithContext(ctx context.Context, method string, params interface{}, result interface{}, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	req, err := NewRequest(method, params)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if err := c.encoder.Encode(req); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	// Without this deadline the read below has nothing to stop it, so a
	// daemon that accepts and never answers hangs the caller forever.
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("arm call timeout: %w", err)
	}
	interruptDone := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		noteDeadline(c.conn.SetReadDeadline(time.Now()), "interrupt call")
		close(interruptDone)
	})
	defer func() {
		if !stopInterrupt() {
			<-interruptDone
		}
		noteDeadline(c.conn.SetReadDeadline(time.Time{}), "clear call deadline")
	}()

	if !c.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.scanner.Err(); err != nil {
			if isReadTimeout(err) {
				return fmt.Errorf("read response: %w", &CallTimeoutError{
					Method:          method,
					TimeoutDuration: timeout,
					Err:             err,
				})
			}
			return fmt.Errorf("read response: %w", err)
		}
		return fmt.Errorf("read response: connection closed")
	}

	var resp Response
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if resp.Error != nil {
		return resp.Error
	}

	if result != nil && resp.Result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}
	}

	return nil
}

// Close disconnects from the server.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Subscribe opens a dedicated connection and subscribes to events for a run.
// Returns an event channel, a cancel function (to stop and clean up), and an error.
// The channel is closed when the run completes, the connection drops, or cancel is called.
func Subscribe(socketPath string, params *SubscribeParams) (<-chan Event, func(), error) {
	return SubscribeContext(context.Background(), socketPath, params)
}

// SubscribeContext is Subscribe with cancellation support while establishing
// the subscription.
func SubscribeContext(ctx context.Context, socketPath string, params *SubscribeParams) (<-chan Event, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	conn, err := dialEndpoint(socketPath)
	if err != nil {
		return nil, nil, err
	}
	encoder := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// Send subscribe request.
	req, err := NewRequest(MethodSubscribe, params)
	if err != nil {
		closers.Quiet(conn)
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := encoder.Encode(req); err != nil {
		closers.Quiet(conn)
		return nil, nil, fmt.Errorf("send request: %w", err)
	}

	// Read initial response.
	interruptDone := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		noteDeadline(conn.SetReadDeadline(time.Now()), "interrupt subscribe")
		close(interruptDone)
	})
	if deadline, ok := ctx.Deadline(); ok {
		// The caller asked for a bound on the subscribe handshake. Failing to
		// arm it would leave the read below waiting past that bound.
		if err := conn.SetReadDeadline(deadline); err != nil {
			if !stopInterrupt() {
				<-interruptDone
			}
			closers.Quiet(conn)
			return nil, nil, fmt.Errorf("arm subscribe deadline: %w", err)
		}
	}
	if !scanner.Scan() {
		if !stopInterrupt() {
			<-interruptDone
		}
		closers.Quiet(conn)
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := scanner.Err(); err != nil {
			return nil, nil, fmt.Errorf("read response: %w", err)
		}
		return nil, nil, fmt.Errorf("read response: connection closed")
	}
	if !stopInterrupt() {
		<-interruptDone
	}
	noteDeadline(conn.SetReadDeadline(time.Time{}), "clear subscribe deadline")
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		closers.Quiet(conn)
		return nil, nil, fmt.Errorf("parse response: %w", err)
	}
	if resp.Error != nil {
		closers.Quiet(conn)
		return nil, nil, resp.Error
	}

	// Stream events.
	ch := make(chan Event, 64)
	done := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			closers.Quiet(conn)
		})
	}

	go func() {
		defer close(ch)
		for scanner.Scan() {
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				continue // skip malformed events
			}
			select {
			case ch <- event:
			case <-done:
				return
			}
		}
	}()

	return ch, cancel, nil
}

// noteDeadline reports a read deadline that could not be set or cleared. These
// calls run on the way into a cancellation or on the way out of a completed
// one, where there is no error return left to use and the connection is the
// caller's only channel back. The next read on it surfaces the consequence.
func noteDeadline(err error, op string) {
	if err != nil {
		slog.Warn("set ipc read deadline failed", "op", op, "error", err)
	}
}

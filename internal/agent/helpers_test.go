package agent

import (
	"io"
	"testing"
)

// writeStub sends one stub HTTP response body. A short write means the client
// hung up mid-turn, which is the adapter behavior these tests assert against,
// so it is reported rather than dropped. It runs on the server's goroutine,
// where t.Errorf is allowed and t.Fatalf is not.
func writeStub(t *testing.T, w io.Writer, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write stub response: %v", err)
	}
}

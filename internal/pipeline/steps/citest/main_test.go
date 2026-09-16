package citest

import (
	"fmt"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

func TestMain(m *testing.M) {
	// Leaving it set would let that config reach every git call these tests
	// make, which is the leak this drops.
	if err := os.Unsetenv("GIT_CONFIG_COUNT"); err != nil {
		fmt.Fprintf(os.Stderr, "unset GIT_CONFIG_COUNT: %v\n", err)
		os.Exit(1)
	}
	cleanup, err := stepstest.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init fake CLI helper: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup fake CLI helper: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// guardGateControl is the one CLI policy for commands that can mutate pipeline
// ownership. It runs before command-specific resource opening, flag-driven
// auto-approval, daemon admission, or any Git/DB mutation.
func guardGateControl(cmd *cobra.Command) error {
	if !mutatesPipelineControl(cmd) {
		return nil
	}
	result, err := classifyGateControlCaller(cmd.Context())
	if err != nil {
		return err
	}
	if !result.Nested {
		return nil
	}
	return emitGateContextRefusal(cmd, result)
}

func mutatesPipelineControl(cmd *cobra.Command) bool {
	path := cmd.CommandPath()
	switch path {
	case "no-mistakes", "no-mistakes init", "no-mistakes eject", "no-mistakes rerun",
		"no-mistakes axi run", "no-mistakes axi respond", "no-mistakes axi abort",
		"no-mistakes daemon start", "no-mistakes daemon stop", "no-mistakes daemon restart",
		"no-mistakes update":
		return true
	case "no-mistakes sync", "no-mistakes axi sync":
		check, err := cmd.Flags().GetBool("check")
		return err != nil || !check
	default:
		return false
	}
}

func classifyGateControlCaller(ctx context.Context) (gatecontext.Result, error) {
	p, err := paths.New()
	if err != nil {
		return gatecontext.Result{}, fmt.Errorf("resolve paths: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return gatecontext.Result{}, fmt.Errorf("resolve current directory: %w", err)
	}
	marker := gatecontext.MarkerPresent()
	alive, runningErr := daemon.IsRunning(p)
	if alive && runningErr != nil {
		return gatecontext.Result{}, fmt.Errorf("inspect daemon for gate execution context: %w", runningErr)
	}
	// A version-mismatched daemon is fully alive but reports not-alive. The
	// local-only fallback below has no authenticated peer to ask about step
	// ancestry, so classifying from cwd alone there is weaker: a step-spawned
	// agent outside its managed worktree can read as not-nested and mutate gate
	// ownership. gate_context is version-exempt on the wire precisely so the
	// skewed daemon can still be asked; query it rather than degrading. The one
	// deliberate exception is handled below, for daemons that predate the
	// method entirely.
	skewed := ipc.IsVersionMismatch(runningErr)
	if alive || skewed {
		result, err := queryDaemonGateContext(p, cwd, marker)
		switch {
		case err == nil:
			return result, nil
		// A daemon old enough to predate gate_context itself cannot answer, and
		// it reports no version at all, which is the whole population this
		// fallback is for. Refusing them would deadlock the very commands that
		// resolve the skew, so classify locally exactly as for an absent daemon.
		// Containment IS weaker for that one population than it would be with a
		// peer answer, and that is the accepted price of a reachable remedy.
		// Any daemon that DOES report a version and still lacks gate_context is
		// a future one that retired the method; degrading containment to the
		// peerless cwd-only inspector for it would widen this into a bypass.
		case reportsNoProtocolVersion(runningErr) && ipc.IsMethodNotFound(err):
		default:
			return gatecontext.Result{}, err
		}
	}
	// A stale/non-socket endpoint can make IsRunning return an error together
	// with alive=false. There is no authenticated peer to query in that state;
	// continue with canonical read-only topology so lifecycle repair commands
	// retain their existing compatibility.

	// No active daemon means there cannot be an active step ancestry. Consult
	// the existing DB read-only so canonical topology still requires a
	// registered gate and authorization itself performs no schema or state write.
	var database *db.DB
	if opened, openErr := db.OpenReadOnly(p.DB()); openErr == nil {
		database = opened
		defer database.Close()
	} else if !os.IsNotExist(openErr) {
		return gatecontext.Result{}, fmt.Errorf("open gate registry read-only: %w", openErr)
	}
	return (gatecontext.Inspector{DB: database, Paths: p}).Inspect(ctx, gatecontext.Request{CWD: cwd, MarkerPresent: marker})
}

// reportsNoProtocolVersion reports whether err describes a peer that carried no
// protocol version at all, which is how every daemon predating the handshake
// reads. A peer on any declared version is a deliberate later protocol, not a
// pre-handshake one, so it never qualifies for compatibility fallbacks.
func reportsNoProtocolVersion(err error) bool {
	var mismatch *ipc.VersionMismatchError
	return errors.As(err, &mismatch) && mismatch.RemoteRole == ipc.RoleDaemon && mismatch.Remote == 0
}

// queryDaemonGateContext asks the running daemon to classify this caller. Its
// error is always wrapped; callers distinguish "this daemon has no such method"
// with ipc.IsMethodNotFound, which unwraps.
func queryDaemonGateContext(p *paths.Paths, cwd string, marker bool) (gatecontext.Result, error) {
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return gatecontext.Result{}, fmt.Errorf("connect to daemon for gate execution context: %w", err)
	}
	defer client.Close()
	var wire ipc.GateContextResult
	if err := client.Call(ipc.MethodGateContext, &ipc.GateContextParams{CWD: cwd, MarkerPresent: marker}, &wire); err != nil {
		return gatecontext.Result{}, fmt.Errorf("classify gate execution context: %w", err)
	}
	return gatecontext.Result{
		Nested:           wire.Nested,
		ManagedGit:       wire.ManagedGit,
		AgentDescendant:  wire.AgentDescendant,
		DaemonDescendant: wire.DaemonDescendant,
		MarkerPresent:    wire.MarkerPresent,
		RunID:            wire.RunID,
		Phase:            wire.Phase,
	}, nil
}

func emitGateContextRefusal(cmd *cobra.Command, result gatecontext.Result) error {
	errorFields := []toon.Field{
		{Key: "code", Value: gatecontext.ErrorCode},
		{Key: "message", Value: "refusing pipeline control from an active no-mistakes validation step"},
	}
	if result.RunID != "" {
		errorFields = append(errorFields, toon.Field{Key: "run", Value: result.RunID})
	}
	if result.Phase != "" {
		errorFields = append(errorFields, toon.Field{Key: "phase", Value: string(result.Phase)})
	}
	allowed := []string{"no-mistakes axi status", "no-mistakes axi logs --step <phase>", "no-mistakes doctor"}
	if result.RunID != "" {
		allowed[0] = "no-mistakes axi status --run " + result.RunID
		allowed[1] = "no-mistakes axi logs --run " + result.RunID + " --step <phase>"
	}
	emitDoc(cmd,
		toon.Field{Key: "error", Value: toon.NewObject(errorFields...)},
		toon.Field{Key: "note", Value: "The enclosing executor owns validation, push, PR, and CI. This step must return only its assigned phase."},
		toon.Field{Key: "allowed", Value: allowed},
		toon.Field{Key: "help", Value: []string{"Return control to the outer executor; do not initialize, start, reattach, rerun, respond to, synchronize, abort, or eject a pipeline from this step."}},
	)
	return &exitError{code: 1}
}

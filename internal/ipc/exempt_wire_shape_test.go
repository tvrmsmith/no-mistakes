package ipc_test

import (
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The three version-exempt meta methods are reachable across a protocol skew,
// so peers on different versions exchange these payloads with no negotiation
// left to catch a shape change. This pins their serialized form.
//
// gate_context is the one that matters most: its payload IS the containment
// verdict, so a renamed field decodes as nested=false on the peer and lets a
// nested caller mutate gate ownership. That drift fails OPEN, which is why it
// is pinned rather than trusted to review.
func TestExemptMethodWireShapesArePinned(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  string
	}{
		{
			name:  "health result",
			value: ipc.HealthResult{Status: "ok", ProtocolVersion: 7},
			want:  `{"status":"ok","protocol_version":7}`,
		},
		{
			name:  "health params",
			value: ipc.HealthParams{},
			want:  `{}`,
		},
		{
			name:  "shutdown params",
			value: ipc.ShutdownParams{},
			want:  `{}`,
		},
		{
			name:  "shutdown result",
			value: ipc.ShutdownResult{OK: true},
			want:  `{"ok":true}`,
		},
		{
			name:  "gate context params",
			value: ipc.GateContextParams{CWD: "/w", MarkerPresent: true},
			want:  `{"cwd":"/w","marker_present":true}`,
		},
		{
			name: "gate context result",
			value: ipc.GateContextResult{
				Nested:           true,
				ManagedGit:       true,
				AgentDescendant:  true,
				DaemonDescendant: true,
				MarkerPresent:    true,
				RunID:            "run-1",
				Phase:            types.StepReview,
			},
			want: `{"nested":true,"managed_git":true,"agent_descendant":true,"daemon_descendant":true,"marker_present":true,"run_id":"run-1","phase":"review"}`,
		},
		// The hook replies are not exempt methods, but they carry the same
		// cross-version contract: a current hook reads the daemon's version off
		// them with no negotiation ahead of it. Renaming the tag makes every
		// reply decode as version 0, so the pre-receive gate refuses every push
		// against a perfectly current daemon.
		{
			name:  "admit push result",
			value: ipc.AdmitPushResult{Context: ipc.GateContextResult{Nested: true}, ProtocolVersion: 7},
			want:  `{"context":{"nested":true},"protocol_version":7}`,
		},
		{
			name:  "push received result",
			value: ipc.PushReceivedResult{RunID: "run-1", ProtocolVersion: 7},
			want:  `{"run_id":"run-1","protocol_version":7}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("wire shape drifted:\n got %s\nwant %s", encoded, tc.want)
			}
		})
	}
}

// A peer on another version decodes these payloads with its own copy of the
// struct, so every field must survive a round trip through the pinned JSON
// above rather than merely being present in the encoder's output.
func TestExemptGateContextResultDecodesEveryVerdictField(t *testing.T) {
	const wire = `{"nested":true,"managed_git":true,"agent_descendant":true,"daemon_descendant":true,"marker_present":true,"run_id":"run-1","phase":"review"}`

	var decoded ipc.GateContextResult
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := ipc.GateContextResult{
		Nested:           true,
		ManagedGit:       true,
		AgentDescendant:  true,
		DaemonDescendant: true,
		MarkerPresent:    true,
		RunID:            "run-1",
		Phase:            types.StepReview,
	}
	if decoded != want {
		t.Fatalf("decoded = %+v, want %+v", decoded, want)
	}
}

package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestRestartExemptCommit is a table test on the predicate alone: given a
// commit's changed paths and the trusted exempt-path list, does the commit
// carry nothing a validation gate needs to see again.
func TestRestartExemptCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		paths       []string
		exemptPaths []string
		want        bool
	}{
		{name: "empty_paths", paths: nil, want: false},
		{name: "readme", paths: []string{"README.md"}, want: true},
		{name: "docs_subtree", paths: []string{"docs/guide.md"}, want: true},
		{name: "docs_nested_subtree", paths: []string{"docs/api/v1/spec.md"}, want: true},
		{name: "root_txt", paths: []string{"notes.txt"}, want: true},
		{name: "nested_txt", paths: []string{"internal/testdata/fixture.txt"}, want: true},
		{name: "license", paths: []string{"LICENSE"}, want: true},
		{name: "license_dotted", paths: []string{"LICENSE.md"}, want: true},
		{name: "copying", paths: []string{"COPYING"}, want: true},
		{name: "notice", paths: []string{"NOTICE"}, want: true},
		{name: "agents_md", paths: []string{"AGENTS.md"}, want: false},
		{name: "claude_md", paths: []string{"CLAUDE.md"}, want: false},
		{name: "nested_agents_md", paths: []string{"docs/AGENTS.md"}, want: false},
		{name: "readme_and_agents_md", paths: []string{"README.md", "AGENTS.md"}, want: false},
		{name: "readme_and_code", paths: []string{"README.md", "main.go"}, want: false},
		{name: "code_only", paths: []string{"main.go"}, want: false},
		{name: "nil_exempt_paths", paths: []string{"README.md"}, exemptPaths: nil, want: false},
		{name: "empty_exempt_paths", paths: []string{"README.md"}, exemptPaths: []string{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exempt := tc.exemptPaths
			if exempt == nil && tc.name != "nil_exempt_paths" && tc.name != "empty_exempt_paths" {
				exempt = config.DefaultRestartExemptPaths
			}
			if got := restartExemptCommit(tc.paths, exempt); got != tc.want {
				t.Fatalf("restartExemptCommit(%v, %v) = %v, want %v", tc.paths, exempt, got, tc.want)
			}
		})
	}
}

// Package claudetrust reads Claude Code's per-workspace trust decision.
//
// Claude Code records whether a workspace has been through its interactive
// trust dialog in a projects map inside its user config file (normally
// ~/.claude.json). no-mistakes runs its gate agent against a bare gate
// repository checked out at a path under ~/.no-mistakes/repos, and when that
// path carries no accepted trust entry, Claude Code discards the repo's
// project-scoped permission entries (permissions.allow, permissions.ask,
// permissions.deny, permissions.additionalDirectories from
// .claude/settings.json and .claude/settings.local.json) and, when it prints
// anything at all about it, writes a warning to stderr.
//
// That warning is NOT the "every tool call sits unapproved" story it might
// look like. no-mistakes launches claude with --dangerously-skip-permissions
// unless the operator pinned their own permission mode (see buildArgs in
// internal/agent/claude.go), and under that bypass a dropped allow/ask/deny
// entry changes nothing about whether a tool call proceeds - permission
// checking itself is off. The one category that still costs the run under
// bypass is permissions.additionalDirectories: bypass grants approval, not
// extra read roots, so a dropped entry there really does shrink what the
// agent can read. Warning.BitesUnderBypass reports exactly this split.
//
// This package is read-only by hard constraint: it must NEVER write to
// Claude Code's config file. A trust decision belongs to the operator, and
// forging one into that file would fake a consent no-mistakes was never
// given. Its job is limited to detecting the untrusted-workspace condition
// and telling the operator, in their own words, how to grant it.
package claudetrust

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/text/unicode/norm"
)

// ConfigPath returns Claude Code's user config file.
//
// CLAUDE_CONFIG_DIR is Claude Code's own relocation mechanism, but it is
// undocumented and unverified here: this is a best-effort fallback, tried
// only when the relocated file actually exists on disk. The documented
// home of the file is ~/.claude.json, which is what every other case
// resolves to.
func ConfigPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		if candidate := filepath.Join(dir, ".claude.json"); fileExists(candidate) {
			return candidate, nil
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// CanonicalWorkspace renders path the way Claude Code stores its projects
// key, so a lookup compares like with like.
//
// Claude Code derives its projects key by walking up from the run directory
// for the git root, resolving a linked worktree's .git file to its
// commondir, and returning the commondir itself when its basename is not
// literally ".git" - which is why a bare gate repo named <id>.git is the key
// rather than the run worktree. Whatever that walk lands on, Claude Code
// stores it realpath'd and NFC-normalized. A byte-for-byte compare against
// that stored key therefore reports a trusted gate as untrusted whenever
// NM_HOME sits under a symlink (macOS /tmp -> /private/tmp) or a path
// carries decomposable Unicode, so every comparison in this package goes
// through this function first.
func CanonicalWorkspace(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	return norm.NFC.String(path)
}

// Config is Claude Code's parsed workspace-trust state.
type Config struct {
	present  bool
	projects map[string]bool
}

type rawProject struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
}

type rawConfig struct {
	Projects map[string]rawProject `json:"projects"`
}

// Load reads the config at path. A missing file is not an error: it returns
// a present-false Config, since an operator who has never run Claude Code
// interactively has trusted nothing. Unknown keys, at either the top level
// or within a project entry, are ignored rather than rejected. Project keys
// are canonicalized on load, so Trusted/Untrusted can compare canonically
// without redoing the filesystem work on every lookup.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	projects := make(map[string]bool, len(raw.Projects))
	for workspace, entry := range raw.Projects {
		projects[CanonicalWorkspace(workspace)] = entry.HasTrustDialogAccepted
	}
	return &Config{present: true, projects: projects}, nil
}

// Present reports whether the config file was found and parsed.
func (c *Config) Present() bool {
	return c != nil && c.present
}

// Trusted reports whether workspace has accepted Claude Code's trust dialog.
func (c *Config) Trusted(workspace string) bool {
	if c == nil {
		return false
	}
	return c.projects[CanonicalWorkspace(workspace)]
}

// Untrusted returns the subset of workspaces that are not trusted, in the
// same order they were given. A nil receiver treats every workspace as
// untrusted, since no config means no trust decisions have been recorded.
func (c *Config) Untrusted(workspaces []string) []string {
	var untrusted []string
	for _, workspace := range workspaces {
		if !c.Trusted(workspace) {
			untrusted = append(untrusted, workspace)
		}
	}
	return untrusted
}

// Warning is a parsed untrusted-workspace warning.
type Warning struct {
	Category  string // "" when the line names none
	Workspace string // "" when the line names none
}

// untrustedWorkspaceSentence is the fixed abort signal in Claude Code's
// warning; the category and workspace path are optional decoration that only
// sharpen the remedy and the bypass judgment.
var untrustedWorkspaceSentence = regexp.MustCompile(`(?i)this workspace has not been trusted`)

// untrustedWorkspaceCategory extracts the dropped setting category, one of
// permissions.allow, permissions.deny, permissions.ask, or
// permissions.additionalDirectories.
var untrustedWorkspaceCategory = regexp.MustCompile(`(?i)\b(permissions\.(?:allow|deny|ask|additionalDirectories))\b`)

// untrustedWorkspacePath extracts the projects["<path>"] key from the same
// warning, when present. The match is case-insensitive but the captured
// path is returned exactly as written.
var untrustedWorkspacePath = regexp.MustCompile(`(?i)projects\["([^"]+)"\]`)

// ParseUntrustedWorkspaceStderr matches Claude Code's untrusted-workspace
// warning on one line of CLI stderr. Matching is anchored on the stable
// middle of the template, "this workspace has not been trusted": the count,
// the "entry"/"entries" pluralization, the category, and the source list
// are all free to vary between Claude Code versions, and the console.error
// that prints the line is itself conditional in the binary, so a rule can be
// dropped with nothing printed at all. This is therefore a best-effort
// detector, not a complete one.
func ParseUntrustedWorkspaceStderr(line string) (Warning, bool) {
	if !untrustedWorkspaceSentence.MatchString(line) {
		return Warning{}, false
	}
	var w Warning
	if m := untrustedWorkspaceCategory.FindStringSubmatch(line); m != nil {
		w.Category = m[1]
	}
	if m := untrustedWorkspacePath.FindStringSubmatch(line); m != nil {
		w.Workspace = m[1]
	}
	return w, true
}

// BitesUnderBypass reports whether this dropped category still costs the run
// when claude launched with --dangerously-skip-permissions. no-mistakes adds
// that flag by default (see buildArgs in internal/agent/claude.go), and under
// it permission checking itself is off, so a discarded permissions.allow,
// permissions.ask, or permissions.deny entry changes nothing about whether a
// tool call proceeds. permissions.additionalDirectories is different: bypass
// grants approval, not extra read roots, so losing it really does shrink what
// the agent can read. An unnamed category (Category == "") also reports
// false: a false abort kills a working run, a missed one only restores
// today's behavior, and the doctor preflight already covers the standing
// condition.
func (w Warning) BitesUnderBypass() bool {
	return w.Category == "permissions.additionalDirectories"
}

// Remedy renders the operator instruction for an untrusted workspace.
// workspace and configPath both fall back to a generic description when
// empty, since ParseUntrustedWorkspaceStderr's path capture is optional and
// ConfigPath can fail.
func Remedy(workspace, configPath string) string {
	place := workspace
	if place == "" {
		place = "the gate repository"
	}

	setClause := fmt.Sprintf(`projects["%s"].hasTrustDialogAccepted to true`, workspace)
	if workspace == "" {
		setClause = "that path's hasTrustDialogAccepted to true"
	}

	target := configPath
	if target == "" {
		target = "Claude Code's user config"
	}

	return fmt.Sprintf("run claude interactively in %s once and accept the trust dialog, or set %s in %s", place, setClause, target)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

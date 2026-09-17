package claudetrust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPath_UsesConfigDirWhenFileExists(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(configFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	if got != configFile {
		t.Errorf("ConfigPath() = %q, want %q", got, configFile)
	}
}

func TestConfigPath_FallsBackToHomeWhenConfigDirFileMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	want := filepath.Join(home, ".claude.json")
	if got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigPath_DefaultsToHomeClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	want := filepath.Join(home, ".claude.json")
	if got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

const sampleConfigJSON = `{"projects":{"/a":{"hasTrustDialogAccepted":true},"/b":{"hasTrustDialogAccepted":false},"/c":{}}}`

func TestLoad_TrustedProjectIsTrusted(t *testing.T) {
	path := writeConfig(t, sampleConfigJSON)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !c.Present() {
		t.Error("Present() = false, want true")
	}

	cases := map[string]bool{"/a": true, "/b": false, "/c": false, "/d": false}
	for workspace, want := range cases {
		if got := c.Trusted(workspace); got != want {
			t.Errorf("Trusted(%q) = %v, want %v", workspace, got, want)
		}
	}
}

func TestLoad_MissingFileIsPresentFalse(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c == nil {
		t.Fatal("Load() = nil, want non-nil *Config")
	}
	if c.Present() {
		t.Error("Present() = true, want false")
	}
	if c.Trusted("anything") {
		t.Error(`Trusted("anything") = true, want false`)
	}
}

func TestLoad_InvalidJSONIsError(t *testing.T) {
	path := writeConfig(t, `{`)

	c, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want non-nil error")
	}
	if c != nil {
		t.Errorf("Load() = %+v, want nil", c)
	}
}

func TestLoad_EmptyObjectIsPresentWithNoTrust(t *testing.T) {
	path := writeConfig(t, `{}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !c.Present() {
		t.Error("Present() = false, want true")
	}
	if c.Trusted("anything") {
		t.Error(`Trusted("anything") = true, want false`)
	}
}

func TestLoad_UnknownKeysAreIgnored(t *testing.T) {
	path := writeConfig(t, `{"numStartups":42,"projects":{"/a":{"hasTrustDialogAccepted":true,"allowedTools":[]}}}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !c.Trusted("/a") {
		t.Error(`Trusted("/a") = false, want true`)
	}
}

// This line is a verbatim fixture taken from
// ~/.no-mistakes/logs/01M1FAF1H15SSVAHRHKDEY6BBG/review.log line 292.
const untrustedWorkspaceStderrFixture = `Ignoring 8 permissions.allow entries from .claude/settings.json: this workspace has not been trusted. Run Claude Code interactively here once and accept the trust dialog, or set projects["/Users/trevor.smith/.no-mistakes/repos/871d740473c0.git"].hasTrustDialogAccepted: true in /Users/trevor.smith/.claude.json.`

func TestParseUntrustedWorkspaceStderr_VerbatimFixtureWithPath(t *testing.T) {
	w, ok := ParseUntrustedWorkspaceStderr(untrustedWorkspaceStderrFixture)
	if !ok {
		t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
	}
	want := Warning{Category: "permissions.allow", Workspace: "/Users/trevor.smith/.no-mistakes/repos/871d740473c0.git"}
	if w != want {
		t.Errorf("ParseUntrustedWorkspaceStderr() = %+v, want %+v", w, want)
	}
	if w.BitesUnderBypass() {
		t.Error("BitesUnderBypass() = true, want false")
	}
}

func TestParseUntrustedWorkspaceStderr_AdditionalDirectoriesSingularAndTwoSources(t *testing.T) {
	line := `Ignoring 1 permissions.additionalDirectories entry from .claude/settings.json and .claude/settings.local.json: this workspace has not been trusted. Run Claude Code interactively here once and accept the trust dialog, or set projects["/x/y.git"].hasTrustDialogAccepted: true in /home/o/.claude.json.`

	w, ok := ParseUntrustedWorkspaceStderr(line)
	if !ok {
		t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
	}
	want := Warning{Category: "permissions.additionalDirectories", Workspace: "/x/y.git"}
	if w != want {
		t.Errorf("ParseUntrustedWorkspaceStderr() = %+v, want %+v", w, want)
	}
	if !w.BitesUnderBypass() {
		t.Error("BitesUnderBypass() = false, want true")
	}
}

func TestParseUntrustedWorkspaceStderr_LiteralClaudeSettingsSourceFallback(t *testing.T) {
	line := `Ignoring 3 permissions.deny entries from .claude/ settings: this workspace has not been trusted.`

	w, ok := ParseUntrustedWorkspaceStderr(line)
	if !ok {
		t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
	}
	want := Warning{Category: "permissions.deny", Workspace: ""}
	if w != want {
		t.Errorf("ParseUntrustedWorkspaceStderr() = %+v, want %+v", w, want)
	}
	if w.BitesUnderBypass() {
		t.Error("BitesUnderBypass() = true, want false")
	}
}

func TestParseUntrustedWorkspaceStderr_TrustSentenceAloneNamesNoCategory(t *testing.T) {
	// The trust sentence alone is a match; category and workspace are optional
	// decoration, so their absence still yields ok == true with a zero Warning.
	// An unnamed category never bites under bypass: a false abort kills a
	// working run, a missed one only restores today's behavior, and the
	// doctor preflight already covers the standing condition.
	line := `this workspace has not been trusted. Run Claude Code interactively here once and accept the trust dialog.`

	w, ok := ParseUntrustedWorkspaceStderr(line)
	if !ok {
		t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
	}
	if w != (Warning{}) {
		t.Errorf("ParseUntrustedWorkspaceStderr() = %+v, want zero Warning", w)
	}
	if w.BitesUnderBypass() {
		t.Error("BitesUnderBypass() = true, want false")
	}
}

func TestParseUntrustedWorkspaceStderr_NonMatches(t *testing.T) {
	cases := []string{
		"",
		"Ignoring 8 permissions.allow entries from .claude/settings.json: something else entirely.",
		`the operator set projects["/x/y.git"].hasTrustDialogAccepted: true`,
		"API Error: Stream idle timeout - no chunks received",
	}
	for _, line := range cases {
		w, ok := ParseUntrustedWorkspaceStderr(line)
		if ok || w != (Warning{}) {
			t.Errorf("ParseUntrustedWorkspaceStderr(%q) = (%+v, %v), want (Warning{}, false)", line, w, ok)
		}
	}
}

func TestParseUntrustedWorkspaceStderr_CaseInsensitiveMatchCanonicalizesCategoryAndPreservesPathCase(t *testing.T) {
	line := `IGNORING 8 PERMISSIONS.ALLOW ENTRIES FROM .claude/settings.json: THIS WORKSPACE HAS NOT BEEN TRUSTED. RUN CLAUDE CODE INTERACTIVELY HERE ONCE AND ACCEPT THE TRUST DIALOG, OR SET PROJECTS["/Users/Trevor/repo.git"].hasTrustDialogAccepted: true IN /Users/Trevor/.claude.json.`

	w, ok := ParseUntrustedWorkspaceStderr(line)
	if !ok {
		t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
	}
	want := Warning{Category: CategoryAllow, Workspace: "/Users/Trevor/repo.git"}
	if w != want {
		t.Errorf("ParseUntrustedWorkspaceStderr() = %+v, want %+v", w, want)
	}
}

// A case-varied additionalDirectories line is the one that must still bite:
// it is the only category the abort path exists for, and folding the capture
// to its canonical spelling is what keeps BitesUnderBypass from silently
// downgrading it to an inert report.
func TestParseUntrustedWorkspaceStderr_CaseVariedAdditionalDirectoriesStillBitesUnderBypass(t *testing.T) {
	line := `Ignoring 1 Permissions.AdditionalDirectories entry from .claude/settings.json: this workspace has not been trusted.`

	w, ok := ParseUntrustedWorkspaceStderr(line)
	if !ok {
		t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
	}
	if w.Category != CategoryAdditionalDirectories {
		t.Errorf("Category = %q, want %q", w.Category, CategoryAdditionalDirectories)
	}
	if !w.BitesUnderBypass() {
		t.Error("BitesUnderBypass() = false, want true for a case-varied additionalDirectories drop")
	}
}

// A single line naming two dropped categories must report the one that bites
// under bypass, whichever order they appear in: reporting the first would
// withhold the abort the fail-fast exists for.
func TestParseUntrustedWorkspaceStderr_MultipleCategoriesPrefersTheBitingOne(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{
			"additionalDirectories second",
			`Ignoring 8 permissions.allow and 1 permissions.additionalDirectories entries from .claude/settings.json: this workspace has not been trusted.`,
		},
		{
			"additionalDirectories first",
			`Ignoring 1 permissions.additionalDirectories and 8 permissions.allow entries from .claude/settings.json: this workspace has not been trusted.`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, ok := ParseUntrustedWorkspaceStderr(tt.line)
			if !ok {
				t.Fatal("ParseUntrustedWorkspaceStderr() ok = false, want true")
			}
			if w.Category != CategoryAdditionalDirectories {
				t.Errorf("Category = %q, want %q", w.Category, CategoryAdditionalDirectories)
			}
			if !w.BitesUnderBypass() {
				t.Error("BitesUnderBypass() = false, want true when additionalDirectories is among the dropped categories")
			}
		})
	}
}

// Two raw keys that canonicalize to the same path must resolve to trusted when
// either one is accepted. Map iteration order is random, so a last-writer-wins
// collapse made this verdict flap between runs against an unchanged config.
func TestLoad_DuplicateCanonicalKeysPreferTrust(t *testing.T) {
	// Two spellings of one absent path: CanonicalWorkspace leaves an
	// unresolvable path alone but still cleans it, so both collapse to the same
	// key without depending on the filesystem's own normalization.
	lookup := "/no-such-root/871d740473c0.git"
	variant := "/no-such-root//./871d740473c0.git"

	for _, tt := range []struct {
		name     string
		accepted string
		refused  string
	}{
		{"accepted entry written first", lookup, variant},
		{"accepted entry written second", variant, lookup},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, `{"projects":{`+
				`"`+jsonEscape(tt.accepted)+`":{"hasTrustDialogAccepted":true},`+
				`"`+jsonEscape(tt.refused)+`":{"hasTrustDialogAccepted":false}}}`)

			// Repeated because the defect was random map iteration order, so a
			// single pass could pass by luck.
			for i := 0; i < 50; i++ {
				c, err := Load(path)
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				if !c.Trusted(lookup) {
					t.Fatalf("Trusted(%q) = false on iteration %d, want true: one accepted entry is decisive", lookup, i)
				}
			}
		})
	}
}

func TestRemedy_WorkspaceAndConfigPathGiven(t *testing.T) {
	got := Remedy("/x/y.git", "/home/o/.claude.json")
	want := `run claude interactively in /x/y.git once and accept the trust dialog, or set projects["/x/y.git"].hasTrustDialogAccepted to true in /home/o/.claude.json`
	if got != want {
		t.Errorf("Remedy() = %q, want %q", got, want)
	}
}

func TestRemedy_EmptyWorkspace(t *testing.T) {
	got := Remedy("", "/home/o/.claude.json")
	want := `run claude interactively in the gate repository once and accept the trust dialog, or set that path's hasTrustDialogAccepted to true in /home/o/.claude.json`
	if got != want {
		t.Errorf("Remedy() = %q, want %q", got, want)
	}
}

func TestRemedy_EmptyConfigPath(t *testing.T) {
	got := Remedy("/x/y.git", "")
	want := `run claude interactively in /x/y.git once and accept the trust dialog, or set projects["/x/y.git"].hasTrustDialogAccepted to true in Claude Code's user config`
	if got != want {
		t.Errorf("Remedy() = %q, want %q", got, want)
	}
}

func TestConfig_NilReceiver(t *testing.T) {
	var c *Config

	if c.Present() {
		t.Error("Present() = true, want false")
	}
	if c.Trusted("anything") {
		t.Error(`Trusted("anything") = true, want false`)
	}

	ws := []string{"/a", "/b"}
	got := c.Untrusted(ws)
	if len(got) != len(ws) {
		t.Fatalf("Untrusted() = %v, want %v", got, ws)
	}
	for i, w := range ws {
		if got[i] != w {
			t.Errorf("Untrusted()[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestUntrusted_ReturnsUntrustedInOrder(t *testing.T) {
	path := writeConfig(t, sampleConfigJSON)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got := c.Untrusted([]string{"/a", "/b", "/c", "/d"})
	want := []string{"/b", "/c", "/d"}
	if len(got) != len(want) {
		t.Fatalf("Untrusted() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Untrusted()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestUntrusted_Nil(t *testing.T) {
	path := writeConfig(t, sampleConfigJSON)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := c.Untrusted(nil); got != nil {
		t.Errorf("Untrusted(nil) = %v, want nil", got)
	}
}

func TestTrusted_ComparesCanonicallyThroughASymlink(t *testing.T) {
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// The config stores the realpath, as Claude Code itself does; the lookup
	// is done through the symlink, as NM_HOME does on macOS (/tmp -> /private/tmp).
	path := writeConfig(t, `{"projects":{"`+jsonEscape(real)+`":{"hasTrustDialogAccepted":true}}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !c.Trusted(link) {
		t.Errorf("Trusted(%q) = false, want true (canonicalizes through symlink to %q)", link, real)
	}
}

func TestCanonicalWorkspace_NFCNormalizes(t *testing.T) {
	// "e" + combining acute accent U+0301 (NFD) vs precomposed "é" (NFC).
	decomposed := "/tmp/caf" + "é"
	precomposed := "/tmp/café"

	if got := CanonicalWorkspace(decomposed); got != CanonicalWorkspace(precomposed) {
		t.Errorf("CanonicalWorkspace(%q) = %q, want %q", decomposed, got, CanonicalWorkspace(precomposed))
	}
}

func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b[1 : len(b)-1])
}

func TestUntrusted_AllTrustedReturnsEmpty(t *testing.T) {
	path := writeConfig(t, `{"projects":{"/a":{"hasTrustDialogAccepted":true}}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := c.Untrusted([]string{"/a"}); len(got) != 0 {
		t.Errorf("Untrusted([]string{\"/a\"}) = %v, want empty", got)
	}
}

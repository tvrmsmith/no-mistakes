package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ExtraLinterMaxFindings bounds how many findings one extra linter may
// contribute to a single lint round. Findings ride the IPC event stream and
// the PR body, so an operator linter pointed at a large legacy tree must not
// be able to make either unbounded.
const ExtraLinterMaxFindings = 50

// ExtraLinter is one linter the operator configured once on this machine and
// wants run in the Lint step of every repository no-mistakes validates,
// alongside whatever the repository configures or the lint agent discovers.
//
// It exists because a personal linter is deliberately invisible to the
// repository: it is delivered by machine-local state (an MSBuild property, a
// binary outside the tree, an adoption registry in ~/.config) precisely so the
// repository commits nothing. The lint agent can only discover repo-committed
// tooling, so without this list such a linter is silently absent from every
// run and reports clean.
//
// It is global-only, and not for convenience: the command runs through `sh -c`
// with the operator's credentials, which is the same class of authority as
// commands.lint. A repository's copy of that field is already read from the
// trusted default branch only; there is no trusted position for this one to
// come from, because the whole point is that no repository declares it. So it
// is accepted from the operator's own ~/.no-mistakes/config.yaml and nowhere
// else. RepoConfig has no `lint` field, so a repository that writes the key
// parses (repo config is deliberately lenient about keys it does not know) and
// contributes nothing.
type ExtraLinter struct {
	// Name identifies the linter in logs and in the finding text. Required,
	// distinct within the list once ExtraLinterIDSlug has folded case and
	// punctuation away, and restricted to path-safe characters so it can be
	// quoted into a log line or a PR body without escaping surprises.
	Name string `yaml:"name"`
	// Command is the shell command, run by `sh -c` in the run worktree with
	// the same environment every other step command gets, plus the
	// NO_MISTAKES_* variables documented in
	// docs/src/content/docs/reference/global-config.md. The diff base reaches
	// the command through NO_MISTAKES_BASE_SHA rather than through template
	// substitution, so no quoting or injection question arises here.
	Command string `yaml:"command"`
	// FindingsPattern turns a report-only linter into findings. Lines of the
	// command's stdout that match it each become one finding.
	//
	// It is required, because a rule set of this kind exits 0 carrying its
	// findings: a linter with no pattern could only ever report nothing, which
	// is the indistinguishable-from-clean outcome this list exists to end.
	// Named capture groups `file`, `line`, and `message` are used for the
	// finding's location and text when present; otherwise the whole matched
	// line is the text.
	FindingsPattern string `yaml:"findings_pattern"`
	// Severity is the severity every matched finding carries, read through
	// EffectiveSeverity. Default "info",
	// which reports on the PR and in `axi status` without parking the step or
	// spending an auto-fix round. "warning" and "error" both gate: they park
	// the Lint step for a decision, exactly as an agent finding of that
	// severity does. That escalation is the operator's explicit choice, never
	// a default.
	Severity string `yaml:"severity"`
}

// Lint is the global lint block, both as written in YAML and as the resolved
// config carries it. There is no resolution work to do: every entry is
// validated at load and each field is read through the accessor that owns its
// normalization.
type Lint struct {
	ExtraLinters []ExtraLinter `yaml:"extra_linters"`
}

// ExtraLinterSeverities are the severities an extra linter's findings may
// carry, in the order a reader should think about them.
var ExtraLinterSeverities = []string{"info", "warning", "error"}

var extraLinterNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// DefaultExtraLinterSeverity keeps an unqualified entry reporting rather than
// gating. Making a personal linter blocking is a separate, deliberate choice.
const DefaultExtraLinterSeverity = "info"

func validateLintRaw(raw Lint) error {
	seen := make(map[string]string, len(raw.ExtraLinters))
	for i, linter := range raw.ExtraLinters {
		name := linter.EffectiveName()
		if name == "" {
			return fmt.Errorf("invalid lint.extra_linters[%d]: name must not be empty", i)
		}
		if !extraLinterNamePattern.MatchString(name) {
			return fmt.Errorf("invalid lint.extra_linters[%d]: name %q must start with a letter or digit and contain only letters, digits, '.', '_' or '-'", i, name)
		}
		// Uniqueness is checked on the slug, not the name, because the slug is
		// what each finding's ID is built from: "my.linter" and "my-linter"
		// are two entries whose findings would otherwise answer to one ID, so
		// a --findings selection would reach the wrong linter's finding.
		slug := ExtraLinterIDSlug(name)
		if other, ok := seen[slug]; ok {
			return fmt.Errorf("invalid lint.extra_linters[%d]: name %q collides with %q; both identify their findings as %q", i, name, other, slug)
		}
		seen[slug] = name
		if linter.EffectiveCommand() == "" {
			return fmt.Errorf("invalid lint.extra_linters[%d] (%s): command must not be empty", i, name)
		}
		pattern := linter.EffectivePattern()
		if pattern == "" {
			return fmt.Errorf("invalid lint.extra_linters[%d] (%s): findings_pattern must not be empty; this linter reports its findings on a clean exit, so without a pattern it can only ever report nothing", i, name)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid lint.extra_linters[%d] (%s): findings_pattern does not compile: %w", i, name, err)
		}
		if severity := strings.TrimSpace(linter.Severity); severity != "" && !validExtraLinterSeverity(severity) {
			return fmt.Errorf("invalid lint.extra_linters[%d] (%s): severity %q must be one of %s", i, name, severity, strings.Join(ExtraLinterSeverities, ", "))
		}
	}
	return nil
}

// ExtraLinterIDSlug is the single owner of how a linter's name becomes the
// stable part of its findings' IDs. Load-time uniqueness and the Lint step's
// ID construction both read it, so two entries can never be accepted that
// would then name their findings identically.
func ExtraLinterIDSlug(name string) string {
	var b strings.Builder
	dashed := true
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dashed = false
			continue
		}
		if !dashed {
			b.WriteByte('-')
			dashed = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func validExtraLinterSeverity(severity string) bool {
	for _, candidate := range ExtraLinterSeverities {
		if severity == candidate {
			return true
		}
	}
	return false
}

// EffectiveSeverity is the single owner of the unset-severity default, so a
// caller that builds an ExtraLinter without going through config loading
// still gets the reporting-not-gating default rather than an empty severity
// that renders as an unclassified finding.
func (l ExtraLinter) EffectiveSeverity() string {
	if severity := strings.TrimSpace(l.Severity); severity != "" {
		return severity
	}
	return DefaultExtraLinterSeverity
}

// EffectiveName, EffectiveCommand, and EffectivePattern own the normalization
// of the three free-text fields, so validation and the Lint step read the same
// string for each without a second resolved copy of the block existing.
func (l ExtraLinter) EffectiveName() string { return strings.TrimSpace(l.Name) }

func (l ExtraLinter) EffectiveCommand() string { return strings.TrimSpace(l.Command) }

func (l ExtraLinter) EffectivePattern() string { return strings.TrimSpace(l.FindingsPattern) }

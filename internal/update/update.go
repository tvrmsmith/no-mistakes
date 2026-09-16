package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/termout"
)

const (
	appName            = "no-mistakes"
	repoName           = "kunchenguid/no-mistakes"
	backgroundFlag     = "--update-check"
	noUpdateCheckEnv   = "NO_MISTAKES_NO_UPDATE_CHECK"
	checksumsAssetName = "checksums.txt"
	cacheTTL           = 24 * time.Hour
	maxAPIResponseSize = 5 << 20
	maxDownloadSize    = 100 << 20
	maxExtractedSize   = 100 << 20
)

var allowInsecureDownloads bool
var currentGOOS = runtime.GOOS
var daemonIsRunning = daemon.IsRunning
var daemonExecutablePath = runningDaemonExecutablePath
var daemonStop = daemon.Stop
var daemonStart = daemon.Start
var windowsExecutablePathForPID = defaultWindowsExecutablePathForPID

type platformSpec struct {
	GOOS   string
	GOARCH string
}

type updater struct {
	appName            string
	currentVersion     string
	platform           platformSpec
	manifestURL        string
	httpClient         *http.Client
	cachePath          string
	executablePath     string
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	outPrinter         *termout.Printer
	errPrinter         *termout.Printer
	now                func() time.Time
	spawnBackground    func(currentVersion string) error
	resetDaemon        func() error
	paths              *paths.Paths
	disableBackground  bool
	noColor            bool
	includePrereleases bool
	assumeYes          bool
	force              bool
}

type RunOptions struct {
	Beta  bool
	Yes   bool
	Force bool
	Stdin io.Reader
}

func Run(ctx context.Context, stdout, stderr io.Writer, opts RunOptions) error {
	u, err := defaultUpdater(stdout, stderr)
	if err != nil {
		return err
	}
	u.includePrereleases = opts.Beta
	u.assumeYes = opts.Yes
	u.force = opts.Force
	if opts.Stdin != nil {
		u.stdin = opts.Stdin
	}
	return u.run(ctx)
}

func MaybeHandleBackgroundCheck(args []string) (bool, error) {
	if len(args) != 2 || args[0] != backgroundFlag {
		return false, nil
	}
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return true, err
	}
	u.currentVersion = args[1]
	return true, u.refreshCache(context.Background())
}

func MaybeNotifyAndCheck(args []string, stderr io.Writer) {
	u, err := defaultUpdater(io.Discard, stderr)
	if err != nil {
		return
	}
	u.maybeNotifyAndCheck(args)
}

func CachedLatestVersion() string {
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return ""
	}
	return u.cachedLatestVersion()
}

func defaultUpdater(stdout, stderr io.Writer) (*updater, error) {
	p, err := paths.New()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	return &updater{
		appName:         appName,
		currentVersion:  buildinfo.CurrentVersion(),
		platform:        platformSpec{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		manifestURL:     defaultManifestURL(repoName),
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		cachePath:       p.UpdateCheckFile(),
		executablePath:  execPath,
		stdin:           os.Stdin,
		stdout:          stdout,
		stderr:          stderr,
		now:             time.Now,
		paths:           p,
		spawnBackground: defaultSpawnBackground,
		resetDaemon: func() error {
			return defaultResetDaemon(p)
		},
	}, nil
}

func (u *updater) refreshCache(ctx context.Context) error {
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	return writeCache(u.cachePath, &checkCache{
		CheckedAt:     u.now(),
		LatestVersion: plan.LatestVersion,
	})
}

func (u *updater) maybeNotifyAndCheck(args []string) {
	if u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return
	}
	// Informational commands must be side-effect-free probes: `update` and the
	// background refresh must not re-enter it, and a version query (`--version`
	// / `-v`) must never print a notice or spawn a background refresh so that
	// supervision scripts can call it as an innocuous health check (#401).
	if len(args) > 0 && (args[0] == "update" || args[0] == backgroundFlag || args[0] == "--version" || args[0] == "-v") {
		return
	}
	cache := readCache(u.cachePath)
	if cache != nil {
		cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
		if err == nil && cmp < 0 {
			// A notice on an ordinary command must stay innocuous (#401), so a
			// failed write is recorded rather than raised.
			u.errOut().Printf("%sA new version of %s is available: %s -> %s\nRun \"%s update\" to update%s\n", u.yellow(), u.appName, u.currentVersion, cache.LatestVersion, u.appName, u.reset())
		}
	}
	if cacheStale(cache, u.currentVersion, u.now()) && u.spawnBackground != nil {
		_ = u.spawnBackground(u.currentVersion)
	}
}

func (u *updater) cachedLatestVersion() string {
	if u == nil || u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return ""
	}
	cache := readCache(u.cachePath)
	if cache == nil {
		return ""
	}
	cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
	if err != nil || cmp >= 0 {
		return ""
	}
	return cache.LatestVersion
}

func (u *updater) run(ctx context.Context) error {
	if isDevVersion(u.currentVersion) {
		u.out().Printf("self-update unavailable for development builds (%s)\n", u.currentVersion)
		return u.writeErr()
	}
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	if err := writeCache(u.cachePath, &checkCache{CheckedAt: u.now(), LatestVersion: plan.LatestVersion}); err != nil {
		return err
	}
	if !plan.UpdateAvailable {
		u.out().Printf("%s is already up to date (%s)\n", u.appName, u.currentVersion)
		return u.writeErr()
	}
	if err := u.confirmActiveRunsBeforeUpdate(); err != nil {
		return err
	}
	if err := u.ensureDaemonUsesCurrentExecutable(); err != nil {
		return err
	}

	archiveData, err := u.downloadAsset(ctx, plan.Archive.BrowserDownloadURL, maxDownloadSize)
	if err != nil {
		return err
	}
	checksumsData, err := u.downloadAsset(ctx, plan.Checksums.BrowserDownloadURL, maxDownloadSize)
	if err != nil {
		return err
	}
	checksums, err := parseChecksums(checksumsData)
	if err != nil {
		return err
	}
	want, ok := checksums[plan.ArchiveName]
	if !ok {
		return fmt.Errorf("checksum not found for %s", plan.ArchiveName)
	}
	if err := verifyChecksum(archiveData, want); err != nil {
		return err
	}
	binaryData, err := u.extractBinary(archiveData)
	if err != nil {
		return err
	}
	if err := replaceExecutable(u.executablePath, binaryData); err != nil {
		return err
	}
	if u.resetDaemon != nil {
		parkedNotice := u.parkedNoticeAtRestart()
		if err := u.resetDaemon(); err != nil {
			var resetErr *daemonResetError
			if errors.As(err, &resetErr) && resetErr.daemonOffline {
				return fmt.Errorf("updated %s to %s, but daemon is offline: %w", u.appName, plan.LatestVersion, err)
			}
			return fmt.Errorf("updated %s to %s, but failed to reset daemon: %w", u.appName, plan.LatestVersion, err)
		}
		// The daemon really was stopped and restarted, so the preservation
		// promise is finally true for this invocation.
		u.errOut().Print(parkedNotice)
	}
	u.out().Printf("updated %s from %s to %s\n", u.appName, u.currentVersion, plan.LatestVersion)
	return u.writeErr()
}

// out and errOut are the update's two output streams, latched so a failed
// write reaches run's return value instead of leaving the command reporting a
// completed update the operator never read. Each is built once, because the
// latch has to accumulate across every line the update prints.
func (u *updater) out() *termout.Printer {
	if u.outPrinter == nil {
		w := io.Writer(io.Discard)
		if u.stdout != nil {
			w = u.stdout
		}
		u.outPrinter = termout.New(w)
	}
	return u.outPrinter
}

func (u *updater) errOut() *termout.Printer {
	if u.errPrinter == nil {
		w := io.Writer(io.Discard)
		if u.stderr != nil {
			w = u.stderr
		}
		u.errPrinter = termout.New(w)
	}
	return u.errPrinter
}

// writeErr reports the first failed write on either stream.
func (u *updater) writeErr() error {
	return errors.Join(u.out().Err(), u.errOut().Err())
}

func (u *updater) yellow() string {
	if u.noColor {
		return ""
	}
	return "\033[33m"
}

func (u *updater) reset() string {
	if u.noColor {
		return ""
	}
	return "\033[0m"
}

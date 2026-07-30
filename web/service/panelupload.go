package service

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/random"
)

// Update from a local file: the operator picks a vpn-ui binary in the browser and the
// panel installs it, instead of fetching the release from GitHub. Same installer, same
// rollback copy, same restart; only the source of the bytes differs, which is why this
// ends in installPanelBinary rather than repeating the swap sequence.
//
// It is deliberately TWO requests. The first uploads and INSPECTS, and answers with the
// version it read out of the file itself; the second applies what inspection staged.
// Nothing is swapped until the operator has been shown real versions, so the
// confirmation is about the binary on disk rather than about a filename, which anyone
// can mistype.

const (
	// MaxPanelUploadSize caps an uploaded binary. The release asset is around 315MB
	// (it embeds Xray, the geo databases and every bundled daemon), so this leaves
	// room to grow while still refusing a body that could fill the filesystem the
	// panel's DB lives on.
	MaxPanelUploadSize = 1 << 30

	// Direction of the staged binary relative to the running one.
	PanelUploadUpgrade   = "upgrade"
	PanelUploadDowngrade = "downgrade"
	PanelUploadSame      = "same"
	PanelUploadUnknown   = "unknown"

	// A version probe should answer instantly; anything slower is not a vpn-ui binary
	// behaving normally and must not hold a request open.
	panelVersionProbeTimeout = 15 * time.Second
	panelVersionProbeMaxOut  = 4 << 10

	// How long a staged file waits for its apply call. The two steps are one operator
	// action seconds apart, so anything older is an abandoned upload: it gets deleted
	// rather than installed, and it is the size of a whole panel binary.
	stagedPanelTTL = 30 * time.Minute
)

// ErrNoStagedPanelBinary reports an apply with nothing staged: the upload failed, the
// panel restarted in between, or a second tab already consumed it.
var ErrNoStagedPanelBinary = errors.New("no uploaded binary is waiting to be applied")

// StagedPanelInfo is what the operator confirms against: both versions, which way the
// swap goes, and the token naming the exact file those versions were read from.
type StagedPanelInfo struct {
	Token     string `json:"token"`
	Current   string `json:"current"`
	New       string `json:"new"`
	Direction string `json:"direction"`
	Size      int64  `json:"size"`
}

// One staging slot, guarded. The token is what ties an apply call to the file the
// inspect call actually measured: without it, a second upload landing between the two
// steps would be installed under the first one's version report, which is precisely
// the confirmation this flow exists to make trustworthy.
var (
	stagedPanelMu   sync.Mutex
	stagedPanelPath string
	stagedPanelInfo StagedPanelInfo
	stagedPanelAt   time.Time
)

// panelVersionPattern is what a version line has to look like. It is the cheapest test
// that separates "a vpn-ui binary" from "some other ELF that printed its usage": the
// panel answers -v with a bare dotted version and nothing else.
var panelVersionPattern = regexp.MustCompile(`^v?\d+(\.\d+)*$`)

// StagePanelBinary writes an uploaded binary next to the running one, checks it is
// something this host can actually exec, reads its version, and holds it for a
// following apply. It does NOT install anything.
//
// The staging path is a sibling of the running binary so the eventual install is a
// rename within one filesystem, which is what makes the swap atomic. /tmp would not be:
// it is frequently a different mount, and os.Rename across mounts fails.
func StagePanelBinary(src io.Reader, declaredSize int64) (StagedPanelInfo, error) {
	var info StagedPanelInfo

	// Advisory only: a chunked upload declares nothing, and a client can lie. The real
	// enforcement is the LimitReader below, which counts what actually arrives.
	if declaredSize > MaxPanelUploadSize {
		return info, fmt.Errorf("that file is %s, larger than the %s limit",
			humanBytes(declaredSize), humanBytes(MaxPanelUploadSize))
	}
	// A download-driven update owns the same paths and ends in the same swap, so the
	// two must not overlap.
	if panelUpdateInFlight.Load() {
		return info, fmt.Errorf("a panel update is already in progress")
	}

	exe, err := panelExecutablePath()
	if err != nil {
		return info, err
	}
	staged := exe + ".upload"

	// Discard anything a previous attempt left behind before writing: a half-uploaded
	// file from a dropped connection must never be what a later apply installs.
	DiscardStagedPanelBinary()

	out, err := os.OpenFile(staged, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return info, fmt.Errorf("cannot stage the upload next to the panel binary: %w", err)
	}
	// One read limit past the cap, so a body that lies about its length is caught by
	// what actually arrived rather than by its Content-Length header.
	written, copyErr := io.Copy(out, io.LimitReader(src, MaxPanelUploadSize+1))
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		_ = os.Remove(staged)
		return info, fmt.Errorf("upload failed: %w", copyErr)
	case closeErr != nil:
		_ = os.Remove(staged)
		return info, fmt.Errorf("upload failed: %w", closeErr)
	case written > MaxPanelUploadSize:
		_ = os.Remove(staged)
		return info, fmt.Errorf("that file is larger than the %s limit", humanBytes(MaxPanelUploadSize))
	case written == 0:
		_ = os.Remove(staged)
		return info, errors.New("that file is empty")
	}

	// Cheapest checks first, and nothing is executed until both have passed. Same gate
	// the downloader uses: a wrong-arch or non-ELF file renamed over the running binary
	// would brick the panel, because the restart fails with exec-format-error and there
	// is no longer a working binary to serve a retry.
	if !IsCompatibleBinary(staged) {
		_ = os.Remove(staged)
		return info, fmt.Errorf("that file is not a %s Linux binary", runtime.GOARCH)
	}
	// A pure parse, no execution: a shell script, a stripped C binary or somebody
	// else's ELF is refused before the probe below ever runs it.
	if !isGoBinary(staged) {
		_ = os.Remove(staged)
		return info, errors.New("that file is not a Go binary, so it is not a vpn-ui panel")
	}

	version, err := panelBinaryVersion(staged)
	if err != nil {
		_ = os.Remove(staged)
		return info, err
	}

	current := config.GetVersion()
	info = StagedPanelInfo{
		Token:     random.Seq(32),
		Current:   current,
		New:       version,
		Direction: comparePanelVersions(version, current),
		Size:      written,
	}

	stagedPanelMu.Lock()
	stagedPanelPath, stagedPanelInfo, stagedPanelAt = staged, info, time.Now()
	stagedPanelMu.Unlock()

	logger.Infof("panel update: staged an uploaded binary, v%s -> v%s (%s)",
		current, version, info.Direction)
	return info, nil
}

// ApplyStagedPanelBinary installs the file a previous StagePanelBinary accepted, named
// by the token that inspection returned, and restarts the panel. Returns once the
// restart has been handed off, exactly as UpdatePanel does.
func ApplyStagedPanelBinary(token string) error {
	stagedPanelMu.Lock()
	staged, info, at := stagedPanelPath, stagedPanelInfo, stagedPanelAt
	// Cleared up front, whatever happens next: a staged file must be installable
	// exactly once, so a double-submit cannot swap the binary twice.
	stagedPanelPath, stagedPanelInfo, stagedPanelAt = "", StagedPanelInfo{}, time.Time{}
	stagedPanelMu.Unlock()

	if staged == "" || info.Token == "" {
		return ErrNoStagedPanelBinary
	}
	// The token is not a secret, it only pins WHICH file the confirmation was about.
	// A mismatch still has to refuse: otherwise a second upload landing between
	// inspect and apply gets installed under the first one's version report, and the
	// operator confirmed a swap that is not the one happening.
	if token != info.Token {
		_ = os.Remove(staged)
		return errors.New("the staged upload no longer matches this confirmation, upload the file again")
	}
	if time.Since(at) > stagedPanelTTL {
		_ = os.Remove(staged)
		return fmt.Errorf("the uploaded binary expired after %s, upload it again", stagedPanelTTL)
	}
	if _, err := os.Stat(staged); err != nil {
		return ErrNoStagedPanelBinary
	}

	exe, err := panelExecutablePath()
	if err != nil {
		_ = os.Remove(staged)
		return err
	}

	if !panelUpdateInFlight.CompareAndSwap(false, true) {
		_ = os.Remove(staged)
		return fmt.Errorf("a panel update is already in progress")
	}
	resetUpdateCounters()

	// installPanelBinary hands off to a detached restart on success, so the in-flight
	// flag is left set on purpose: it dies with this process and blocks a duplicate
	// update during the restart window. Only a failure releases it.
	if err := installPanelBinary(staged, exe); err != nil {
		setUpdateProgress(updatePhaseError, 0)
		panelUpdateInFlight.Store(false)
		return err
	}
	logger.Infof("panel update: applied uploaded binary v%s", info.New)
	return nil
}

// StagedPanelBinaryInfo reports what is waiting, if anything.
func StagedPanelBinaryInfo() (StagedPanelInfo, bool) {
	stagedPanelMu.Lock()
	defer stagedPanelMu.Unlock()
	return stagedPanelInfo, stagedPanelPath != ""
}

// DiscardStagedPanelBinary drops a staged upload and its file. Called when the
// operator backs out of the confirmation, and before staging a new one.
func DiscardStagedPanelBinary() {
	stagedPanelMu.Lock()
	staged := stagedPanelPath
	stagedPanelPath, stagedPanelInfo, stagedPanelAt = "", StagedPanelInfo{}, time.Time{}
	stagedPanelMu.Unlock()

	if staged != "" {
		_ = os.Remove(staged)
		return
	}
	// Nothing tracked in memory does not mean nothing on disk: a panel that restarted
	// mid-flow left its staging file behind, and it is not ours to keep.
	if exe, err := panelExecutablePath(); err == nil {
		_ = os.Remove(exe + ".upload")
	}
}

// CleanStagedPanelUpload removes a staged upload left behind by a panel that died, or
// was itself updated, between the inspect and apply steps. The token that made such a
// file installable lived only in that process's memory, so anything still on disk at
// startup is orphaned by definition, and it is the size of a whole panel binary.
// Called once at startup, next to the orphan-daemon reap.
func CleanStagedPanelUpload() {
	exe, err := panelExecutablePath()
	if err != nil {
		return
	}
	stale := exe + ".upload"
	if _, err := os.Stat(stale); err != nil {
		return
	}
	if err := os.Remove(stale); err != nil {
		logger.Warning("panel update: could not remove a stale staged upload:", err)
		return
	}
	logger.Infof("panel update: removed a stale staged upload at %s", stale)
}

// isGoBinary reports whether path carries Go build info. A pure parse with no
// execution, used as the gate in front of the -v probe.
//
// It cannot be used to READ the version: build.sh compiles `go build main.go`, a file
// argument, so the recorded main package is "command-line-arguments" with no module
// and no version, and the panel's own version is a go:embed of config/version with
// nothing around it to anchor a scan on.
func isGoBinary(path string) bool {
	_, err := buildinfo.ReadFile(path)
	return err == nil
}

// panelExecutablePath resolves the running binary through any symlink, which is the
// path the install renames over.
func panelExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot resolve own path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// panelBinaryVersion reads a binary's version by running it with -v.
//
// It has to be exec'd rather than parsed: the version is a go:embed'd string in the
// data section, so debug/buildinfo does not carry it and a byte scan would be guessing.
// Executing an uploaded file is not a new exposure here, because the very next step is
// to install it AS the panel and exec it as root; a caller who can upload can already
// do that, and the endpoint is super-admin only. It is still bounded: a timeout, a
// capped stdout, no inherited environment, and only after the ELF check has passed.
func panelBinaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), panelVersionProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "-v")
	// A deliberately bare environment: the probe needs nothing from ours, and the
	// panel's own variables (VPNUI_*) should not steer a stranger's binary.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Dir = filepath.Dir(path)
	var out cappedBuffer
	out.max = panelVersionProbeMaxOut
	cmd.Stdout = &out
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("that binary did not answer with a version (it hung), so it is not a vpn-ui build")
		}
		return "", errors.New("that binary could not be run to read its version, so it is not a usable vpn-ui build")
	}

	// First line only. -v prints the version and exits, so anything after it is noise
	// from a binary that is not what it claims to be.
	version := strings.TrimSpace(out.buf.String())
	if i := strings.IndexAny(version, "\r\n"); i >= 0 {
		version = strings.TrimSpace(version[:i])
	}
	if !panelVersionPattern.MatchString(version) {
		return "", errors.New("that file did not report a version number, so it is not a vpn-ui binary")
	}
	return strings.TrimPrefix(version, "v"), nil
}

// comparePanelVersions classifies the staged version against the running one. Reuses
// versionNewer so "is this newer" is decided by the same rule the release check
// applies.
//
// Parseability is checked FIRST because versionNewer answers false both for "equal"
// and for "I cannot read this": without the check, an unreadable tag would compare
// equal to everything and be waved through as "same version". And equality is decided
// numerically rather than by string, so 1.8 and 1.8.0 are one version rather than an
// unknown pair the operator gets warned about for no reason.
func comparePanelVersions(staged, current string) string {
	if !parsablePanelVersion(staged) || !parsablePanelVersion(current) {
		return PanelUploadUnknown
	}
	switch {
	case versionNewer(staged, current):
		return PanelUploadUpgrade
	case versionNewer(current, staged):
		return PanelUploadDowngrade
	default:
		return PanelUploadSame
	}
}

// parsablePanelVersion reports whether versionNewer can actually order this string:
// a leading "v" and dotted decimal components, nothing else.
func parsablePanelVersion(v string) bool {
	return panelVersionPattern.MatchString(strings.TrimSpace(v))
}

// cappedBuffer collects at most max bytes and silently drops the rest, so a binary
// that floods stdout cannot make the probe allocate without bound. Writes still
// succeed past the cap: failing them would make the child die of EPIPE and turn a
// chatty binary into an unreadable error.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// humanBytes renders a size for an error message the operator reads once.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

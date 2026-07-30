package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The direction is what the confirmation dialog is built on: get it wrong and an
// operator is either warned about an upgrade or NOT warned about a downgrade, which is
// the case that can leave a newer database under an older panel.
func TestComparePanelVersions(t *testing.T) {
	for _, tc := range []struct {
		staged, current, want string
	}{
		{"1.9.0", "1.8.5", PanelUploadUpgrade},
		{"v1.9.0", "1.8.5", PanelUploadUpgrade},
		{"1.8.5", "1.9.0", PanelUploadDowngrade},
		{"1.8.5", "v1.8.5", PanelUploadSame},
		{"1.8.5", "1.8.5", PanelUploadSame},
		// Trailing zeros are the same version written two ways, not a change.
		{"1.8", "1.8.0", PanelUploadSame},
		{"2.0.0", "1.9.9", PanelUploadUpgrade},
		{"1.10.0", "1.9.0", PanelUploadUpgrade},
		// Unparseable on either side: say so rather than guess a direction.
		{"nightly", "1.8.5", PanelUploadUnknown},
		{"1.8.5", "nightly", PanelUploadUnknown},
	} {
		if got := comparePanelVersions(tc.staged, tc.current); got != tc.want {
			t.Errorf("compare(%q, %q) = %q, want %q", tc.staged, tc.current, got, tc.want)
		}
	}
}

// A binary that does not answer with a version is not a vpn-ui build, and installing
// it would replace the panel with something that cannot come back up.
func TestPanelBinaryVersionRejectsNonPanelOutput(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	t.Run("a bare version is accepted", func(t *testing.T) {
		got, err := panelBinaryVersion(write("good", `echo 1.9.0`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "1.9.0" {
			t.Errorf("got %q, want 1.9.0", got)
		}
	})

	t.Run("a leading v is stripped", func(t *testing.T) {
		got, err := panelBinaryVersion(write("vprefix", `echo v1.9.0`))
		if err != nil || got != "1.9.0" {
			t.Errorf("got %q, %v; want 1.9.0, nil", got, err)
		}
	})

	t.Run("usage text is refused", func(t *testing.T) {
		if _, err := panelBinaryVersion(write("usage", `echo "usage: something [opts]"`)); err == nil {
			t.Error("a binary printing usage was accepted as a vpn-ui build")
		}
	})

	t.Run("silence is refused", func(t *testing.T) {
		if _, err := panelBinaryVersion(write("quiet", `exit 0`)); err == nil {
			t.Error("a binary printing nothing was accepted")
		}
	})

	t.Run("a non-zero exit is refused", func(t *testing.T) {
		if _, err := panelBinaryVersion(write("fails", `echo 1.9.0; exit 3`)); err == nil {
			t.Error("a binary that exited non-zero was accepted")
		}
	})

	t.Run("a missing file is refused", func(t *testing.T) {
		if _, err := panelBinaryVersion(filepath.Join(dir, "nope")); err == nil {
			t.Error("a missing file was accepted")
		}
	})

	// Only the first line counts: -v prints the version and exits, so a binary that
	// keeps talking is not one.
	t.Run("only the first line is read", func(t *testing.T) {
		got, err := panelBinaryVersion(write("chatty", "echo 1.9.0\necho and more"))
		if err != nil || got != "1.9.0" {
			t.Errorf("got %q, %v; want 1.9.0, nil", got, err)
		}
	})
}

// A wrong file must be refused BEFORE anything is staged for install: the guard exists
// so a non-binary can never be renamed over the running panel.
func TestStagePanelBinaryRejectsNonElf(t *testing.T) {
	t.Cleanup(DiscardStagedPanelBinary)

	if _, err := StagePanelBinary(strings.NewReader("#!/bin/sh\necho hi\n"), 18); err == nil {
		t.Fatal("a shell script was accepted as a panel binary")
	}
	if _, ok := StagedPanelBinaryInfo(); ok {
		t.Error("a rejected upload was left staged")
	}

	if _, err := StagePanelBinary(strings.NewReader(""), 0); err == nil {
		t.Error("an empty upload was accepted")
	}

	// The declared size is checked before a single byte is read, so an oversized
	// upload cannot fill the filesystem the panel's DB lives on first.
	if _, err := StagePanelBinary(strings.NewReader("x"), MaxPanelUploadSize+1); err == nil {
		t.Error("an oversized upload was accepted")
	}
}

// Nothing staged must fail loudly rather than install whatever happens to be lying
// around under the .upload name.
func TestApplyWithNothingStaged(t *testing.T) {
	DiscardStagedPanelBinary()
	if err := ApplyStagedPanelBinary("any-token"); err != ErrNoStagedPanelBinary {
		t.Errorf("got %v, want ErrNoStagedPanelBinary", err)
	}
}

// The token is what stops a second upload landing between inspect and apply from
// being installed under the FIRST one's version report: the operator would confirm
// "1.8.5 to 1.9.0" and get somebody else's binary.
func TestApplyRefusesAForeignToken(t *testing.T) {
	t.Cleanup(DiscardStagedPanelBinary)

	exe, err := panelExecutablePath()
	if err != nil {
		t.Skipf("cannot resolve the test binary path: %v", err)
	}
	staged := exe + ".upload"
	if err := os.WriteFile(staged, []byte("not really a binary"), 0o755); err != nil {
		t.Fatalf("seed staged file: %v", err)
	}
	stagedPanelMu.Lock()
	stagedPanelPath = staged
	stagedPanelInfo = StagedPanelInfo{Token: "the-real-token", New: "1.9.0"}
	stagedPanelAt = time.Now()
	stagedPanelMu.Unlock()

	err = ApplyStagedPanelBinary("a-different-token")
	if err == nil {
		t.Fatal("a mismatched token was accepted")
	}
	if !strings.Contains(err.Error(), "no longer matches") {
		t.Errorf("got %q, want the mismatch message", err)
	}
	// And the file it refused is gone, not left waiting for a luckier caller.
	if _, statErr := os.Stat(staged); statErr == nil {
		t.Error("the refused upload was left on disk")
	}
}

// An upload nobody confirmed is an abandoned upload, and it is the size of a whole
// panel binary. It must expire rather than stay installable indefinitely.
func TestApplyRefusesAnExpiredUpload(t *testing.T) {
	t.Cleanup(DiscardStagedPanelBinary)

	exe, err := panelExecutablePath()
	if err != nil {
		t.Skipf("cannot resolve the test binary path: %v", err)
	}
	staged := exe + ".upload"
	if err := os.WriteFile(staged, []byte("stale"), 0o755); err != nil {
		t.Fatalf("seed staged file: %v", err)
	}
	stagedPanelMu.Lock()
	stagedPanelPath = staged
	stagedPanelInfo = StagedPanelInfo{Token: "tok", New: "1.9.0"}
	stagedPanelAt = time.Now().Add(-stagedPanelTTL - time.Minute)
	stagedPanelMu.Unlock()

	err = ApplyStagedPanelBinary("tok")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("got %v, want an expiry refusal", err)
	}
	if _, statErr := os.Stat(staged); statErr == nil {
		t.Error("the expired upload was left on disk")
	}
}

// The probe reads a stranger's stdout, so it must not grow without bound.
func TestCappedBufferTruncates(t *testing.T) {
	c := &cappedBuffer{max: 8}
	n, err := c.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write reported %d, %v; it must accept everything so the child does not die of EPIPE", n, err)
	}
	if got := c.buf.String(); got != "01234567" {
		t.Errorf("kept %q, want the first 8 bytes only", got)
	}
	if _, err := c.Write([]byte("more")); err != nil {
		t.Errorf("a write past the cap must still succeed, got %v", err)
	}
	if c.buf.Len() != 8 {
		t.Errorf("buffer grew to %d past its cap", c.buf.Len())
	}
}

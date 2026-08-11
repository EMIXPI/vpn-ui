package service

import (
	"os"
	"path/filepath"
	"testing"
)

// parseCpuinfoMhz runs the real parser over a fixture file. avgCpuinfoMhz takes
// its path for exactly this reason; production passes the /proc constant.
func parseCpuinfoMhz(t *testing.T, body string) float64 {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return avgCpuinfoMhz(path)
}

func TestCpuMhzFromProcCpuinfoAveragesTheCores(t *testing.T) {
	// Verbatim shape from /proc/cpuinfo, trimmed to the fields that matter. The
	// spread is the point: these are four DIFFERENT current clocks, which is what
	// the rated figure could never show.
	body := `processor	: 0
model name	: 13th Gen Intel(R) Core(TM) i9
cpu MHz		: 4407.429

processor	: 1
model name	: 13th Gen Intel(R) Core(TM) i9
cpu MHz		: 1327.426

processor	: 2
cpu MHz		: 1683.672

processor	: 3
cpu MHz		: 4507.588
`
	got := parseCpuinfoMhz(t, body)
	want := (4407.429 + 1327.426 + 1683.672 + 4507.588) / 4
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("average = %v, want %v", got, want)
	}
}

// arm64 has no "cpu MHz" field at all. 0 means "unknown", and the caller falls
// back to the rated speed; anything else would be invented.
func TestCpuMhzFromProcCpuinfoReportsUnknownWhenTheFieldIsAbsent(t *testing.T) {
	body := `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd evtstrm aes pmull
CPU implementer	: 0x41
`
	if got := parseCpuinfoMhz(t, body); got != 0 {
		t.Errorf("got %v, want 0 for a cpuinfo with no cpu MHz line", got)
	}
}

func TestCpuMhzFromProcCpuinfoSkipsUnparsableAndZeroLines(t *testing.T) {
	body := `processor	: 0
cpu MHz		: 3000.000

processor	: 1
cpu MHz		: not-a-number

processor	: 2
cpu MHz		: 0.000

processor	: 3
cpu MHz		: 1000.000
`
	// Only the two usable lines count; a zero would drag the average down and a
	// parse failure must not be read as 0.
	if got := parseCpuinfoMhz(t, body); got != 2000 {
		t.Errorf("average = %v, want 2000 (3000 and 1000, ignoring the other two)", got)
	}
}

func TestCpuMhzFromProcCpuinfoHandlesAMissingFile(t *testing.T) {
	if got := avgCpuinfoMhz(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

// The sysfs reader, against a fake cpufreq tree: one offline core (unreadable),
// one garbage value, and two good ones.
func TestCpuMhzFromSysfsAveragesOnlineCoresOnly(t *testing.T) {
	dir := t.TempDir()
	mk := func(cpu, body string) string {
		p := filepath.Join(dir, "cpu"+cpu, "cpufreq")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f := filepath.Join(p, "scaling_cur_freq")
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		return f
	}
	mk("0", "800000\n")    // 800 MHz
	mk("1", "4500000\n")   // 4500 MHz
	mk("2", "<unknown>\n") // some drivers answer this; must be skipped, not 0
	unreadable := mk("3", "3000000\n")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	got := avgSysfsMhz(filepath.Join(dir, "cpu[0-9]*", "cpufreq", "scaling_cur_freq"))
	// Running as root defeats the chmod, so accept either the 2-core or the
	// 3-core average rather than making the result depend on who runs the tests.
	two := (800.0 + 4500.0) / 2
	three := (800.0 + 4500.0 + 3000.0) / 3
	if got != two && got != three {
		t.Errorf("average = %v, want %v (cpu3 unreadable) or %v (running as root)", got, two, three)
	}
	if got == 0 {
		t.Error("a tree with two readable cores reported unknown")
	}
}

func TestCpuMhzFromSysfsReportsUnknownWithNoCpufreqTree(t *testing.T) {
	if got := avgSysfsMhz(filepath.Join(t.TempDir(), "cpu[0-9]*", "cpufreq", "scaling_cur_freq")); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

// The live reader must never report the rated speed. This is the regression the
// whole file exists for: the panel showed a cached cpu.Info().Mhz, which on this
// host is 4900 forever.
func TestLiveCpuMhzIsNotTheRatedSpeed(t *testing.T) {
	got := liveCpuMhz()
	if got == 0 {
		t.Skip("this host reports no live clock (no cpufreq sysfs, no cpu MHz)")
	}
	if got < 100 || got > 10000 {
		t.Errorf("live clock %v MHz is outside any plausible range", got)
	}
	raw, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")
	if err != nil {
		return // nothing to compare against
	}
	max := avgSysfsMhz("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")
	if max > 0 && got == max {
		t.Errorf("live clock %v MHz equals the rated maximum (%s): it is not live", got, raw)
	}
}

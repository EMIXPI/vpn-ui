package service

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The CPU's CURRENT clock, as opposed to the one it is rated for.
//
// cpu.Info().Mhz, which the spec readouts use, is the RATED figure on Linux: on
// the machine this was written on it reports 4900, which is exactly
// /sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq, and it never moves. The
// panel cached it on top of that, so a row rendered from it was a constant
// pretending to be a measurement. Measured on the same host one second apart,
// the live values ranged from 798 MHz to 4500 MHz across cores.
//
// AVERAGED over the online cores rather than maxed. A ten-core chip sitting at
// 800 MHz on nine cores and 4.5 GHz on one is mostly idle, and the max would
// report it as flat out. The average is also what desktop monitors show.
//
// The two readers take their path as an argument purely so the tests can drive
// them against a fixture tree; production always passes the constants below.

const (
	// cpuFreqSysfsGlob matches one file per CPU, holding that CPU's current kHz.
	cpuFreqSysfsGlob = "/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq"
	procCpuinfoPath  = "/proc/cpuinfo"
)

// liveCpuMhz returns the current average clock in MHz, or 0 when the host cannot
// report one. 0 is a real answer, not a failure: a container or a VM usually has
// no cpufreq at all and no live clock to read, and the caller is expected to fall
// back to the rated speed rather than render a confident zero.
func liveCpuMhz() float64 {
	if mhz := avgSysfsMhz(cpuFreqSysfsGlob); mhz > 0 {
		return mhz
	}
	return avgCpuinfoMhz(procCpuinfoPath)
}

// avgSysfsMhz averages scaling_cur_freq across the online CPUs.
//
// This is the cpufreq driver's own answer and the first choice. The glob is not
// cached: CPUs can be hotplugged, and a stale list would either miss a new core
// or keep reading a file that has gone. Unreadable entries are skipped, which is
// what an offline core looks like; so is "<unknown>", which some drivers answer
// with and which must not be read as zero.
func avgSysfsMhz(glob string) float64 {
	paths, err := filepath.Glob(glob)
	if err != nil || len(paths) == 0 {
		return 0
	}
	var sum float64
	var n int
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		khz, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
		if err != nil || khz <= 0 {
			continue
		}
		sum += khz / 1000
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// avgCpuinfoMhz averages the "cpu MHz" lines, for a host with no cpufreq sysfs.
//
// One file read rather than one per core, so it is the cheaper of the two, but it
// is second because it is the less reliable: on x86 the kernel fills it from the
// same live counters, while on some virtualised hosts it is a constant, and on
// arm64 the field is absent entirely.
func avgCpuinfoMhz(path string) float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var sum float64
	var n int
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "cpu MHz" {
			continue
		}
		mhz, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || mhz <= 0 {
			continue
		}
		sum += mhz
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

package metrics

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// procSelf exports the handful of process_* series an operator actually alerts
// on, read from /proc/self.
//
// client_golang's NewProcessCollector provided these; the hand-rolled registry
// that replaced it did not, and that was the ONE capability loss in the swap
// that was not cosmetic. process_resident_memory_bytes is the usual memory
// alert and process_start_time_seconds is the usual "did it restart?" alert, so
// dropping them would have broken existing rules silently -- the series simply
// stops existing, and a rule with no series does not fire.
//
// Linux only, by construction: /proc is read directly rather than through a
// dependency. On any other platform this exports nothing rather than guessing,
// which is the same failure mode as a runtime metric this Go release does not
// know about. obsidibot ships in a Linux container, so in practice it is
// always present.
type procSelf struct {
	// pageSize and clockTicks convert /proc's units. Both are fixed for the
	// life of the process.
	pageSize   float64
	clockTicks float64
	// bootTime is the unix time the machine booted, needed to turn the
	// process's start time (expressed in ticks since boot) into a wall clock.
	// Read once: it does not change.
	once     sync.Once
	bootTime float64
}

func newProcSelf() *procSelf {
	return &procSelf{
		pageSize: float64(os.Getpagesize()),
		// USER_HZ is 100 on every Linux platform Go supports. It is not
		// exposed without cgo, and getting it wrong only rescales two
		// counters, so the constant is preferable to a dependency.
		clockTicks: 100,
	}
}

const (
	nameProcessCPU     = "process_cpu_seconds_total"
	nameProcessRSS     = "process_resident_memory_bytes"
	nameProcessVirtual = "process_virtual_memory_bytes"
	nameProcessStart   = "process_start_time_seconds"
	nameProcessFDs     = "process_open_fds"
)

func (p *procSelf) describe() []desc {
	return []desc{
		{name: nameProcessCPU, help: "Total user and system CPU time spent in seconds.", typ: "counter"},
		{name: nameProcessRSS, help: "Resident memory size in bytes.", typ: "gauge"},
		{name: nameProcessVirtual, help: "Virtual memory size in bytes.", typ: "gauge"},
		{name: nameProcessStart, help: "Start time of the process since the unix epoch in seconds.", typ: "gauge"},
		{name: nameProcessFDs, help: "Number of open file descriptors.", typ: "gauge"},
	}
}

func (p *procSelf) collect() []family {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		// Not Linux, or /proc is not mounted. Export nothing rather than
		// zeroes that would look like a healthy idle process.
		return nil
	}

	fields, ok := statFields(string(stat))
	if !ok {
		return nil
	}

	var families []family
	add := func(name, help, typ string, value float64) {
		families = append(families, family{
			d:      desc{name: name, help: help, typ: typ},
			groups: []seriesGroup{{lines: appendSample(nil, name, "", "", value)}},
		})
	}

	// Fields are 1-indexed in proc(5); this slice is 0-indexed and starts at
	// field 1, so utime(14) is index 13.
	utime, uok := parseField(fields, 13)
	stime, sok := parseField(fields, 14)
	if uok && sok {
		add(nameProcessCPU, "Total user and system CPU time spent in seconds.", "counter",
			(utime+stime)/p.clockTicks)
	}
	if vsize, ok := parseField(fields, 22); ok {
		add(nameProcessVirtual, "Virtual memory size in bytes.", "gauge", vsize)
	}
	if rss, ok := parseField(fields, 23); ok {
		add(nameProcessRSS, "Resident memory size in bytes.", "gauge", rss*p.pageSize)
	}
	if starttime, ok := parseField(fields, 21); ok {
		if boot := p.boot(); boot > 0 {
			add(nameProcessStart, "Start time of the process since the unix epoch in seconds.", "gauge",
				boot+starttime/p.clockTicks)
		}
	}
	if fds, ok := openFDs(); ok {
		add(nameProcessFDs, "Number of open file descriptors.", "gauge", fds)
	}
	return families
}

// statFields splits /proc/self/stat, which cannot simply be whitespace-split:
// field 2 is the executable name in parentheses and MAY CONTAIN SPACES AND
// PARENTHESES. Everything after the LAST ')' is safe to split.
func statFields(stat string) ([]string, bool) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 || end+2 > len(stat) {
		return nil, false
	}
	// Re-attach two placeholders so the caller can index by proc(5)'s field
	// numbers without subtracting.
	return append([]string{"pid", "comm"}, strings.Fields(stat[end+2:])...), true
}

func parseField(fields []string, idx int) (float64, bool) {
	if idx >= len(fields) {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[idx], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// boot returns the machine's boot time as a unix timestamp, read once.
func (p *procSelf) boot() float64 {
	p.once.Do(func() {
		stat, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for line := range strings.SplitSeq(string(stat), "\n") {
			if rest, found := strings.CutPrefix(line, "btime "); found {
				if v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64); err == nil {
					p.bootTime = v
				}
				return
			}
		}
	})
	return p.bootTime
}

// openFDs counts entries in /proc/self/fd. The read itself holds a descriptor,
// which is why the count is taken from the returned names rather than from a
// separate stat.
func openFDs() (float64, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	return float64(len(entries)), true
}

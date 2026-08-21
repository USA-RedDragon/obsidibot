package metrics

import (
	"log/slog"
	"runtime"
	"runtime/metrics"
)

// goRuntime exports a deliberately small set of Go runtime series.
//
// client_golang's NewGoCollector exported roughly a hundred, and
// NewProcessCollector another dozen read out of /proc. Almost none of them
// were ever looked at here. This exports the handful that answer the questions
// an operator actually asks of this bot -- is it leaking goroutines, is it
// leaking memory, is it spending its life in GC -- and nothing else.
//
// Every value is read at scrape time from runtime/metrics, which is cheap and,
// unlike runtime.ReadMemStats, does not stop the world.
type goRuntime struct {
	series []runtimeSeries
}

// runtimeSeries maps one runtime/metrics sample onto one exported family.
type runtimeSeries struct {
	name   string
	help   string
	typ    string
	sample string
	// scale multiplies the raw value, for the samples whose units are not the
	// ones the exported name promises. 0 means 1.
	scale float64
}

// newGoRuntime builds the runtime collector.
func newGoRuntime() *goRuntime {
	return &goRuntime{series: []runtimeSeries{
		{
			name:   "go_goroutines",
			help:   "Goroutines that currently exist.",
			typ:    "gauge",
			sample: "/sched/goroutines:goroutines",
		},
		{
			name:   "go_gomaxprocs",
			help:   "Current GOMAXPROCS setting: the number of goroutines that may run simultaneously.",
			typ:    "gauge",
			sample: "/sched/gomaxprocs:threads",
		},
		{
			name:   "go_memstats_alloc_bytes",
			help:   "Bytes of live heap objects.",
			typ:    "gauge",
			sample: "/memory/classes/heap/objects:bytes",
		},
		{
			name:   "go_memstats_alloc_bytes_total",
			help:   "Cumulative bytes allocated for heap objects since the process started.",
			typ:    "counter",
			sample: "/gc/heap/allocs:bytes",
		},
		{
			name:   "go_memstats_sys_bytes",
			help:   "Bytes of memory obtained from the OS.",
			typ:    "gauge",
			sample: "/memory/classes/total:bytes",
		},
		{
			name:   "go_memstats_heap_objects",
			help:   "Number of live heap objects.",
			typ:    "gauge",
			sample: "/gc/heap/objects:objects",
		},
		{
			name:   "go_gc_cycles_total",
			help:   "Completed GC cycles since the process started.",
			typ:    "counter",
			sample: "/gc/cycles/total:gc-cycles",
		},
		{
			name:   "go_gc_cpu_seconds_total",
			help:   "CPU seconds spent in garbage collection since the process started. Compare against go_gomaxprocs times uptime.",
			typ:    "counter",
			sample: "/cpu/classes/gc/total:cpu-seconds",
		},
	}}
}

// goInfoName is the build-identity series. Its one label is the Go version,
// which is fixed for the life of the binary, so it is not a cardinality risk.
const goInfoName = "go_info"

func (gr *goRuntime) describe() []desc {
	out := make([]desc, 0, len(gr.series)+1)
	for _, s := range gr.series {
		out = append(out, desc{name: s.name, help: s.help, typ: s.typ})
	}
	out = append(out, desc{
		name: goInfoName,
		help: "Information about the Go environment this binary was built with.",
		typ:  "gauge",
	})
	return out
}

func (gr *goRuntime) collect() []family {
	samples := make([]metrics.Sample, len(gr.series))
	for i, s := range gr.series {
		samples[i].Name = s.sample
	}
	metrics.Read(samples)

	families := make([]family, 0, len(gr.series)+1)
	for i, s := range gr.series {
		value, ok := sampleValue(samples[i])
		if !ok {
			// A name this Go release does not know about. Dropping the family
			// is better than exporting a zero that looks like a real reading.
			slog.Debug("runtime metric unavailable in this Go release, not exporting it",
				"metric", s.name, "sample", s.sample)
			continue
		}
		if s.scale != 0 {
			value *= s.scale
		}
		families = append(families, family{
			d:      desc{name: s.name, help: s.help, typ: s.typ},
			groups: []seriesGroup{{lines: appendSample(nil, s.name, "", "", value)}},
		})
	}

	version := labelPairs([]string{"version"}, []string{runtime.Version()})
	families = append(families, family{
		d: desc{
			name: goInfoName,
			help: "Information about the Go environment this binary was built with.",
			typ:  "gauge",
		},
		groups: []seriesGroup{{key: version, lines: appendSample(nil, goInfoName, "", version, 1)}},
	})
	return families
}

// sampleValue reads a runtime/metrics sample, reporting whether it holds a
// number this collector can export.
func sampleValue(s metrics.Sample) (float64, bool) {
	switch s.Value.Kind() {
	case metrics.KindUint64:
		return float64(s.Value.Uint64()), true
	case metrics.KindFloat64:
		return s.Value.Float64(), true
	case metrics.KindBad, metrics.KindFloat64Histogram:
		// KindBad is an unsupported name. Histograms are not exported: their
		// bucket layouts are the runtime's, not ours, and runtime/metrics does
		// not give a sum, which the text format requires.
		return 0, false
	default:
		return 0, false
	}
}

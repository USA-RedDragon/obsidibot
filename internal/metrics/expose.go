package metrics

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// exportContentType is the Prometheus text exposition format this package
// writes. Version 0.0.4 is what every Prometheus server and every
// OpenMetrics-capable scraper still accepts, and it is the default
// client_golang served.
const exportContentType = "text/plain; version=0.0.4; charset=utf-8"

// bucketSuffix names the per-bucket series of a histogram.
const bucketSuffix = "_bucket"

// positiveInf is how the text format spells an unbounded upper boundary. It is
// written verbatim rather than through strconv, which would produce "+Inf" for
// a float but "Inf" for some formats.
const positiveInf = "+Inf"

// desc is a metric family's immutable identity: what a scrape sees in its
// "# HELP" and "# TYPE" lines.
type desc struct {
	name string
	help string
	typ  string
}

// seriesGroup is one child's rendered sample lines, kept together so a
// histogram's buckets stay in ascending order after the sort.
//
// key is what the group sorts on: the child's rendered label block. Sorting on
// it makes a scrape byte-identical between runs given the same values, which
// is what makes both diffs and the tests in this package stable.
type seriesGroup struct {
	key   string
	lines []byte
}

// family is one metric family: a name, help and type written exactly once,
// followed by every series that belongs to it.
type family struct {
	d      desc
	groups []seriesGroup
}

// collector is something the registry can gather from. It is unexported on
// purpose: the registry only ever holds collectors this package builds, which
// is the same guarantee the private prometheus.Registry gave.
type collector interface {
	// describe names the families this collector owns, so the registry can
	// reject a duplicate name at construction rather than emit one at scrape.
	describe() []desc
	// collect reads current values and renders them.
	collect() []family
}

// Registry holds a set of collectors and renders them on scrape. It is safe
// for concurrent use, and it is an http.Handler serving the exposition format.
type Registry struct {
	mu         sync.Mutex
	collectors []collector
	names      map[string]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{names: make(map[string]struct{}, 32)}
}

// register adds a collector, panicking on a duplicate or unusable family name.
//
// Both are wiring mistakes, and both are better found by the first test that
// calls New than by a scraper rejecting the output — which is how they would
// otherwise surface, since nothing between here and the scraper looks at a
// metric name at all. client_golang validated names; so does this.
func (r *Registry) register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range c.describe() {
		if !validMetricName(d.name) {
			panic("metrics: " + d.name + " is not a valid metric name")
		}
		if _, taken := r.names[d.name]; taken {
			panic("metrics: " + d.name + " is registered twice")
		}
		r.names[d.name] = struct{}{}
	}
	r.collectors = append(r.collectors, c)
}

// checkLabelNames rejects label names a scraper could not parse, or that would
// collide with something the format owns.
func checkLabelNames(metric string, labelNames []string) {
	seen := make(map[string]struct{}, len(labelNames))
	for _, name := range labelNames {
		if !validLabelName(name) {
			panic("metrics: " + metric + " has an invalid label name " + name)
		}
		if strings.HasPrefix(name, "__") {
			panic("metrics: " + metric + " uses the reserved label name " + name)
		}
		if name == "le" {
			// Harmless on a counter, silently corrupting on a histogram, so it
			// is refused everywhere rather than only where it bites.
			panic("metrics: " + metric + " uses le, which the histogram format owns")
		}
		if _, dup := seen[name]; dup {
			panic("metrics: " + metric + " declares the label " + name + " twice")
		}
		seen[name] = struct{}{}
	}
}

// validMetricName reports whether name matches [a-zA-Z_:][a-zA-Z0-9_:]*, which
// is what the text exposition format allows.
func validMetricName(name string) bool { return validIdent(name, true) }

// validLabelName reports whether name matches [a-zA-Z_][a-zA-Z0-9_]*. A colon
// is reserved for recording rules and is not allowed here.
func validLabelName(name string) bool { return validIdent(name, false) }

func validIdent(s string, allowColon bool) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c == ':' && allowColon:
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// NewCounterVec registers and returns a counter family labelled by labelNames.
func (r *Registry) NewCounterVec(name, help string, labelNames []string) *CounterVec {
	checkLabelNames(name, labelNames)
	cv := &CounterVec{
		d: desc{name: name, help: help, typ: "counter"},
		v: newVec(name, labelNames, func(pairs string) *Counter {
			return &Counter{pairs: pairs}
		}),
	}
	r.register(cv)
	return cv
}

// NewCounter registers and returns a single unlabelled counter. Unlike an
// empty vec it is exposed from the moment it is created, at zero.
func (r *Registry) NewCounter(name, help string) *Counter {
	return r.NewCounterVec(name, help, nil).WithLabelValues()
}

// NewGauge registers and returns a single unlabelled gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	gf := &gaugeFamily{
		d: desc{name: name, help: help, typ: "gauge"},
		g: &Gauge{},
	}
	r.register(gf)
	return gf.g
}

// NewHistogramVec registers and returns a histogram family labelled by
// labelNames. buckets are the inclusive upper boundaries, ascending; the +Inf
// bucket is implicit.
func (r *Registry) NewHistogramVec(name, help string, labelNames []string, buckets []float64) *HistogramVec {
	checkLabelNames(name, labelNames)
	upper := slices.Clone(buckets)
	if !sort.Float64sAreSorted(upper) {
		panic("metrics: " + name + " was given unsorted buckets")
	}
	hv := &HistogramVec{
		d: desc{name: name, help: help, typ: "histogram"},
		v: newVec(name, labelNames, func(pairs string) *Histogram {
			return newHistogram(pairs, upper)
		}),
	}
	r.register(hv)
	return hv
}

// NewHistogram registers and returns a single unlabelled histogram.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	return r.NewHistogramVec(name, help, nil, buckets).WithLabelValues()
}

// Gather renders every registered collector in the Prometheus text exposition
// format.
//
// Families come out sorted by name, and series sorted within their family, so
// two scrapes of the same values are byte-identical.
func (r *Registry) Gather() []byte {
	r.mu.Lock()
	collectors := slices.Clone(r.collectors)
	r.mu.Unlock()

	families := make([]family, 0, len(collectors)+8)
	for _, c := range collectors {
		families = append(families, c.collect()...)
	}
	sort.Slice(families, func(i, j int) bool { return families[i].d.name < families[j].d.name })

	buf := make([]byte, 0, 8192)
	for i := range families {
		f := &families[i]
		// A family with no children is left out entirely rather than emitting
		// a header with nothing under it, which is what client_golang did for
		// a vec nobody has touched yet.
		if len(f.groups) == 0 {
			continue
		}

		buf = append(buf, "# HELP "...)
		buf = append(buf, f.d.name...)
		buf = append(buf, ' ')
		buf = appendEscapedHelp(buf, f.d.help)
		buf = append(buf, '\n')

		buf = append(buf, "# TYPE "...)
		buf = append(buf, f.d.name...)
		buf = append(buf, ' ')
		buf = append(buf, f.d.typ...)
		buf = append(buf, '\n')

		sort.Slice(f.groups, func(a, b int) bool { return f.groups[a].key < f.groups[b].key })
		for _, g := range f.groups {
			buf = append(buf, g.lines...)
		}
	}
	return buf
}

// ServeHTTP writes a scrape.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	body := r.Gather()
	header := w.Header()
	header.Set("Content-Type", exportContentType)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// appendSample writes one complete sample line, newline included:
//
//	name<suffix>{pairs} value
func appendSample(dst []byte, name, suffix, pairs string, value float64) []byte {
	dst = append(dst, name...)
	dst = append(dst, suffix...)
	if pairs != "" {
		dst = append(dst, '{')
		dst = append(dst, pairs...)
		dst = append(dst, '}')
	}
	dst = append(dst, ' ')
	dst = appendFloat(dst, value)
	return append(dst, '\n')
}

// withLe returns a bucket series' label block: the histogram's own labels with
// le appended last, which is the order client_golang emitted and the order
// anyone reading a diff will expect.
func withLe(pairs, boundary string) string {
	le := `le="` + boundary + `"`
	if pairs == "" {
		return le
	}
	return pairs + "," + le
}

// labelPairs renders name="value" pairs sorted by label name, escaped, ready
// to sit between a series' braces.
func labelPairs(names, values []string) string {
	if len(names) == 0 {
		return ""
	}

	order := make([]int, len(names))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return names[order[a]] < names[order[b]] })

	var b strings.Builder
	for n, i := range order {
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(names[i])
		b.WriteString(`="`)
		b.Write(appendEscapedLabelValue(nil, values[i]))
		b.WriteByte('"')
	}
	return b.String()
}

// appendFloat writes a value the way the text format wants it.
//
// 'g' with -1 precision is the shortest representation that parses back to the
// same float64, so a boundary written as 0.005 is read back as 0.005 and a
// counter never drifts through the exposition.
func appendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsInf(f, 1):
		return append(dst, positiveInf...)
	case math.IsInf(f, -1):
		return append(dst, "-Inf"...)
	case math.IsNaN(f):
		return append(dst, "NaN"...)
	}
	return strconv.AppendFloat(dst, f, 'g', -1, 64)
}

// formatFloat is appendFloat as a string, for label values such as le.
func formatFloat(f float64) string {
	return string(appendFloat(nil, f))
}

// appendEscapedHelp escapes a help string: backslash and newline only. A help
// string is not quoted, so a double quote inside it needs no escape.
func appendEscapedHelp(dst []byte, s string) []byte {
	for i := range len(s) {
		switch s[i] {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, s[i])
		}
	}
	return dst
}

// appendEscapedLabelValue escapes a label value: backslash, double quote and
// newline. A label value is quoted, so an unescaped quote would end it early
// and produce a line no scraper can parse.
func appendEscapedLabelValue(dst []byte, s string) []byte {
	for i := range len(s) {
		switch s[i] {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '"':
			dst = append(dst, '\\', '"')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, s[i])
		}
	}
	return dst
}

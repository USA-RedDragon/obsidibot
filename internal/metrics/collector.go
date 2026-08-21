package metrics

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// maxSeriesPerFamily caps how many distinct label combinations one metric
// family may mint.
//
// The package's label-cardinality rule is the real defence; this is the
// backstop for the day someone breaks it anyway. Past the cap a family stops
// minting series and folds every further label combination into one shared
// overflow series, so a leaked player name costs a bounded amount of memory
// and a visible wrong-looking series rather than an unbounded leak that only
// shows up as an OOM days later. Every legitimate family here is far below it:
// the widest is command x result, and the command set is a closed list this
// binary defines.
const maxSeriesPerFamily = 256

// overflowLabel is the value every label takes on the shared series a family
// falls back to once it is past maxSeriesPerFamily. It is deliberately ugly:
// seeing it on a dashboard means a metric is being labelled with something
// unbounded.
const overflowLabel = "__overflow__"

// atomicFloat is a float64 that several goroutines may update at once.
type atomicFloat struct {
	bits atomic.Uint64
}

func (a *atomicFloat) load() float64   { return math.Float64frombits(a.bits.Load()) }
func (a *atomicFloat) store(v float64) { a.bits.Store(math.Float64bits(v)) }
func (a *atomicFloat) add(delta float64) {
	for {
		old := a.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if a.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Counter is a cumulative value that only ever goes up. It is safe for
// concurrent use.
type Counter struct {
	// pairs is this series' label block, already rendered and escaped, e.g.
	// `command="bank",result="ok"`. Empty for an unlabelled counter.
	pairs string
	val   atomicFloat
}

// Inc adds one.
func (c *Counter) Inc() { c.val.add(1) }

// Add adds delta, which must not be negative.
func (c *Counter) Add(delta float64) {
	if delta < 0 {
		panic(fmt.Sprintf("metrics: Counter.Add called with a negative delta (%v); counters only go up", delta))
	}
	c.val.add(delta)
}

// Value returns the current count.
func (c *Counter) Value() float64 { return c.val.load() }

// Gauge is a value that goes up and down. It is safe for concurrent use.
type Gauge struct {
	pairs string
	val   atomicFloat
}

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.val.store(v) }

// Add adds delta, which may be negative.
func (g *Gauge) Add(delta float64) { g.val.add(delta) }

// Sub subtracts delta.
func (g *Gauge) Sub(delta float64) { g.val.add(-delta) }

// Inc adds one.
func (g *Gauge) Inc() { g.val.add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.val.add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return g.val.load() }

// Histogram counts observations into cumulative buckets and tracks their sum
// and count. It is safe for concurrent use.
//
// Unlike prometheus/client_golang this takes a plain mutex rather than
// swapping hot and cold halves. Observations here are at most a few per
// second, and the mutex buys an exactly consistent scrape: buckets, sum and
// count always describe the same set of observations.
type Histogram struct {
	pairs string
	// upper are the inclusive bucket boundaries, ascending, without the
	// implicit +Inf bucket.
	upper []float64

	mu     sync.Mutex
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(pairs string, upper []float64) *Histogram {
	return &Histogram{pairs: pairs, upper: upper, counts: make([]uint64, len(upper))}
}

// Observe records one observation.
func (h *Histogram) Observe(v float64) {
	// The smallest index whose boundary is >= v, which is exactly the bucket v
	// belongs in, since boundaries are inclusive. A value past the last
	// boundary lands only in the implicit +Inf bucket.
	i := sort.SearchFloat64s(h.upper, v)

	h.mu.Lock()
	defer h.mu.Unlock()
	if i < len(h.counts) {
		h.counts[i]++
	}
	h.sum += v
	h.count++
}

// Count returns how many observations have been recorded.
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// vec is the child map shared by every labelled family.
//
// The map is behind a mutex; the children it hands out are lock-free or
// hold their own lock, so the mutex is only ever held for a lookup.
type vec[T any] struct {
	name       string
	labelNames []string
	newChild   func(pairs string) *T

	mu       sync.Mutex
	children map[string]*T
	overflow *T
}

func newVec[T any](name string, labelNames []string, newChild func(pairs string) *T) *vec[T] {
	return &vec[T]{
		name:       name,
		labelNames: labelNames,
		newChild:   newChild,
		children:   make(map[string]*T, 8),
	}
}

// with returns the child for these label values, creating it on first use.
func (v *vec[T]) with(values []string) *T {
	if len(values) != len(v.labelNames) {
		panic(fmt.Sprintf("metrics: %s has %d labels %v but was given %d values %v",
			v.name, len(v.labelNames), v.labelNames, len(values), values))
	}
	// \xff cannot appear in valid UTF-8, so no set of label values can collide
	// with another set by joining differently.
	key := strings.Join(values, "\xff")

	v.mu.Lock()
	defer v.mu.Unlock()
	if child, ok := v.children[key]; ok {
		return child
	}
	if len(v.children) >= maxSeriesPerFamily {
		return v.overflowChild()
	}
	child := v.newChild(labelPairs(v.labelNames, values))
	v.children[key] = child
	return child
}

// overflowChild returns the family's single past-the-cap series. Callers hold
// v.mu.
func (v *vec[T]) overflowChild() *T {
	if v.overflow == nil {
		values := make([]string, len(v.labelNames))
		for i := range values {
			values[i] = overflowLabel
		}
		v.overflow = v.newChild(labelPairs(v.labelNames, values))
		slog.Warn("metric hit its cardinality cap; every further label combination is folded into one overflow series",
			"metric", v.name, "cap", maxSeriesPerFamily, "labels", v.labelNames)
	}
	return v.overflow
}

// snapshot returns every live child, in no particular order. Rendering sorts.
func (v *vec[T]) snapshot() []*T {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*T, 0, len(v.children)+1)
	for _, child := range v.children {
		out = append(out, child)
	}
	if v.overflow != nil {
		out = append(out, v.overflow)
	}
	return out
}

// CounterVec is a family of counters that differ only in their label values.
type CounterVec struct {
	d desc
	v *vec[Counter]
}

// WithLabelValues returns the counter for these label values, in the order the
// family's labels were declared. It panics if the count is wrong, which is a
// programming error rather than something to discover in production.
//
// Label values must come from a small closed set; see the package doc.
func (cv *CounterVec) WithLabelValues(values ...string) *Counter {
	return cv.v.with(values)
}

func (cv *CounterVec) describe() []desc { return []desc{cv.d} }

func (cv *CounterVec) collect() []family {
	children := cv.v.snapshot()
	f := family{d: cv.d, groups: make([]seriesGroup, 0, len(children))}
	for _, child := range children {
		f.groups = append(f.groups, seriesGroup{
			key:   child.pairs,
			lines: appendSample(nil, cv.d.name, "", child.pairs, child.val.load()),
		})
	}
	return []family{f}
}

// HistogramVec is a family of histograms that differ only in their label
// values. Every child shares the same buckets.
type HistogramVec struct {
	d desc
	v *vec[Histogram]
}

// WithLabelValues returns the histogram for these label values. See
// [CounterVec.WithLabelValues].
func (hv *HistogramVec) WithLabelValues(values ...string) *Histogram {
	return hv.v.with(values)
}

func (hv *HistogramVec) describe() []desc { return []desc{hv.d} }

func (hv *HistogramVec) collect() []family {
	children := hv.v.snapshot()
	f := family{d: hv.d, groups: make([]seriesGroup, 0, len(children))}
	for _, child := range children {
		f.groups = append(f.groups, child.group(hv.d.name))
	}
	return []family{f}
}

// group renders one histogram's whole block: every cumulative bucket, the
// +Inf bucket, the sum and the count.
func (h *Histogram) group(name string) seriesGroup {
	h.mu.Lock()
	counts := slices.Clone(h.counts)
	sum, total := h.sum, h.count
	h.mu.Unlock()

	// Rough: enough for a bucket line each plus _sum and _count.
	buf := make([]byte, 0, (len(counts)+3)*(len(name)+len(h.pairs)+32))

	var cumulative uint64
	for i, boundary := range h.upper {
		cumulative += counts[i]
		buf = appendSample(buf, name, bucketSuffix, withLe(h.pairs, formatFloat(boundary)), float64(cumulative))
	}
	buf = appendSample(buf, name, bucketSuffix, withLe(h.pairs, positiveInf), float64(total))
	buf = appendSample(buf, name, "_sum", h.pairs, sum)
	buf = appendSample(buf, name, "_count", h.pairs, float64(total))

	return seriesGroup{key: h.pairs, lines: buf}
}

// gaugeFamily is the collector behind an unlabelled [Gauge].
//
// There is deliberately no exported GaugeVec: nothing in obsidibot labels a
// gauge, and an API nobody calls is an API nobody notices is wrong.
type gaugeFamily struct {
	d desc
	g *Gauge
}

func (gf *gaugeFamily) describe() []desc { return []desc{gf.d} }

func (gf *gaugeFamily) collect() []family {
	return []family{{
		d: gf.d,
		groups: []seriesGroup{{
			key:   gf.g.pairs,
			lines: appendSample(nil, gf.d.name, "", gf.g.pairs, gf.g.val.load()),
		}},
	}}
}

// ExponentialBuckets returns count boundaries starting at start and growing by
// factor, which is the bucket layout obsidibot's latency histograms use.
func ExponentialBuckets(start, factor float64, count int) []float64 {
	if count < 1 {
		panic("metrics: ExponentialBuckets needs at least one bucket")
	}
	if start <= 0 {
		panic("metrics: ExponentialBuckets needs a positive start")
	}
	if factor <= 1 {
		panic("metrics: ExponentialBuckets needs a factor greater than 1")
	}
	buckets := make([]float64, count)
	for i := range buckets {
		buckets[i] = start
		start *= factor
	}
	return buckets
}

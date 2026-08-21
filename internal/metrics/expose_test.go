package metrics_test

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/metrics"
)

// TestExpositionGolden pins the exact bytes of a scrape.
//
// The whole point of replacing client_golang was to keep producing what a
// Prometheus server accepts, and the only way to know that has not drifted is
// to compare against text a human read once and agreed with.
func TestExpositionGolden(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()

	things := r.NewCounterVec("things_total", "Things done, by kind and result.", []string{"kind", "result"})
	// Registered out of order on purpose: the output must still be sorted.
	things.WithLabelValues("b", "ok").Add(2)
	things.WithLabelValues("a", "ok").Inc()

	backlog := r.NewGauge("backlog", "Items waiting.")
	backlog.Set(3)

	lonely := r.NewCounter("lonely_total", "A counter with no labels.")
	lonely.Inc()

	dur := r.NewHistogramVec("dur_seconds", "How long an op took.", []string{"op"}, []float64{0.25, 0.5})
	dur.WithLabelValues("x").Observe(0.25) // first bucket
	dur.WithLabelValues("x").Observe(0.5)  // second bucket
	dur.WithLabelValues("x").Observe(1)    // only +Inf

	// A family nobody has touched emits nothing at all, not a bare header.
	r.NewCounterVec("untouched_total", "Never incremented.", []string{"kind"})

	const want = `# HELP backlog Items waiting.
# TYPE backlog gauge
backlog 3
# HELP dur_seconds How long an op took.
# TYPE dur_seconds histogram
dur_seconds_bucket{op="x",le="0.25"} 1
dur_seconds_bucket{op="x",le="0.5"} 2
dur_seconds_bucket{op="x",le="+Inf"} 3
dur_seconds_sum{op="x"} 1.75
dur_seconds_count{op="x"} 3
# HELP lonely_total A counter with no labels.
# TYPE lonely_total counter
lonely_total 1
# HELP things_total Things done, by kind and result.
# TYPE things_total counter
things_total{kind="a",result="ok"} 1
things_total{kind="b",result="ok"} 2
`

	if got := string(r.Gather()); got != want {
		t.Errorf("scrape mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestExpositionEscaping covers the three characters that break a scrape if
// they go out raw.
func TestExpositionEscaping(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	c := r.NewCounterVec("esc_total", "Help with a \\ backslash\nand a newline.", []string{"v"})
	c.WithLabelValues("a\\b\"c\nd").Inc()

	// A double quote needs no escape in help text: help is not quoted.
	q := r.NewCounterVec("quote_total", `Help with a " quote.`, []string{"v"})
	q.WithLabelValues("plain").Inc()

	const want = `# HELP esc_total Help with a \\ backslash\nand a newline.
# TYPE esc_total counter
esc_total{v="a\\b\"c\nd"} 1
# HELP quote_total Help with a " quote.
# TYPE quote_total counter
quote_total{v="plain"} 1
`

	if got := string(r.Gather()); got != want {
		t.Errorf("escaping mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestBucketBoundariesRoundTrip checks that every le value written for the
// buckets obsidibot actually uses parses back to the identical float64. A
// boundary that does not round-trip silently moves a histogram's buckets
// between the process and the scraper.
func TestBucketBoundariesRoundTrip(t *testing.T) {
	t.Parallel()

	buckets := metrics.ExponentialBuckets(0.005, 2, 12)
	r := metrics.NewRegistry()
	h := r.NewHistogram("dur_seconds", "Latency.", buckets)
	h.Observe(0.007)

	les := bucketBoundaries(t, string(r.Gather()), "dur_seconds")
	if len(les) != len(buckets)+1 {
		t.Fatalf("got %d le values, want %d buckets plus +Inf", len(les), len(buckets))
	}
	if les[len(les)-1] != "+Inf" {
		t.Errorf("last bucket is %q, want +Inf", les[len(les)-1])
	}

	// The exact strings a reader will see in a dashboard query.
	wantText := []string{
		"0.005", "0.01", "0.02", "0.04", "0.08", "0.16",
		"0.32", "0.64", "1.28", "2.56", "5.12", "10.24", "+Inf",
	}
	for i, le := range les {
		if le != wantText[i] {
			t.Errorf("bucket %d rendered as %q, want %q", i, le, wantText[i])
		}
	}

	for i, le := range les[:len(les)-1] {
		back, err := strconv.ParseFloat(le, 64)
		if err != nil {
			t.Fatalf("le %q does not parse: %v", le, err)
		}
		if back != buckets[i] {
			t.Errorf("le %q parses to %v, want %v", le, back, buckets[i])
		}
	}
}

// TestHistogramBucketsAreCumulative checks the counts themselves, since a
// non-cumulative histogram parses fine and is simply wrong.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	h := r.NewHistogram("dur_seconds", "Latency.", []float64{1, 2, 4})
	for _, v := range []float64{0.5, 1, 3, 100} {
		h.Observe(v)
	}

	got := seriesValues(t, string(r.Gather()))
	want := map[string]float64{
		`dur_seconds_bucket{le="1"}`:    2, // 0.5 and 1, boundaries are inclusive
		`dur_seconds_bucket{le="2"}`:    2,
		`dur_seconds_bucket{le="4"}`:    3, // plus 3
		`dur_seconds_bucket{le="+Inf"}`: 4, // plus 100
		"dur_seconds_sum":               104.5,
		"dur_seconds_count":             4,
	}
	for series, wantValue := range want {
		if got[series] != wantValue {
			t.Errorf("%s = %v, want %v", series, got[series], wantValue)
		}
	}
}

// TestGatherIsDeterministic guards the property that makes every other test
// here and every diff of a scrape readable.
func TestGatherIsDeterministic(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	c := r.NewCounterVec("things_total", "Things.", []string{"kind"})
	for _, kind := range []string{"z", "m", "a", "q", "b"} {
		c.WithLabelValues(kind).Inc()
	}

	first := string(r.Gather())
	for range 20 {
		if got := string(r.Gather()); got != first {
			t.Fatalf("two scrapes of unchanged values differ\n%s\n---\n%s", first, got)
		}
	}

	var kinds []string
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "things_total{") {
			kinds = append(kinds, line)
		}
	}
	if !isSorted(kinds) {
		t.Errorf("series are not sorted: %v", kinds)
	}
}

// TestSpecialFloatValues covers the values that have their own spelling.
func TestSpecialFloatValues(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	r.NewGauge("inf_gauge", "Infinite.").Set(math.Inf(1))
	r.NewGauge("neginf_gauge", "Negatively infinite.").Set(math.Inf(-1))
	r.NewGauge("nan_gauge", "Not a number.").Set(math.NaN())
	r.NewGauge("big_gauge", "Large.").Set(1234567890)

	got := string(r.Gather())
	for _, want := range []string{
		"inf_gauge +Inf\n",
		"neginf_gauge -Inf\n",
		"nan_gauge NaN\n",
		"big_gauge 1.23456789e+09\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape is missing %q\n%s", want, got)
		}
	}
}

// TestEveryFamilyHeaderAppearsOnce validates the real registry, runtime series
// included: one HELP and one TYPE per family, every sample line parseable, and
// every histogram complete.
func TestEveryFamilyHeaderAppearsOnce(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.InteractionsTotal.WithLabelValues("bank", metrics.ResultOK).Inc()
	m.InteractionDuration.WithLabelValues("bank").Observe(0.02)
	m.RCONDuration.Observe(0.4)
	m.DBErrorsTotal.Inc()
	m.BankNeedsReview.Set(0)

	validate(t, string(m.Registry.Gather()))
}

// validate parses a scrape the way a scraper would and fails the test on
// anything it would reject.
func validate(t *testing.T, body string) {
	t.Helper()

	helps := map[string]int{}
	types := map[string]string{}
	samples := map[string]bool{}
	families := []string{}

	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			helps[name]++
			families = append(families, name)
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if _, dup := types[name]; dup {
				t.Errorf("family %s has more than one TYPE line", name)
			}
			switch typ {
			case "counter", "gauge", "histogram":
			default:
				t.Errorf("family %s has unknown type %q", name, typ)
			}
			types[name] = typ
		case strings.HasPrefix(line, "#"):
			t.Errorf("unexpected comment line: %q", line)
		default:
			series, value, ok := strings.Cut(line, " ")
			if !ok {
				t.Errorf("sample line has no value: %q", line)
				continue
			}
			if _, err := parseSampleValue(value); err != nil {
				t.Errorf("sample %q has an unparseable value %q: %v", series, value, err)
			}
			if !validSeries(series) {
				t.Errorf("sample line is malformed: %q", line)
			}
			samples[series] = true
		}
	}

	for name, count := range helps {
		if count != 1 {
			t.Errorf("family %s has %d HELP lines, want 1", name, count)
		}
		if _, ok := types[name]; !ok {
			t.Errorf("family %s has a HELP line but no TYPE line", name)
		}
	}
	for name := range types {
		if helps[name] == 0 {
			t.Errorf("family %s has a TYPE line but no HELP line", name)
		}
	}
	if !isSorted(families) {
		t.Errorf("families are not sorted: %v", families)
	}

	// Every histogram must carry a +Inf bucket, a sum and a count, or a
	// scraper will accept the family and compute nonsense from it.
	for name, typ := range types {
		if typ != "histogram" {
			continue
		}
		var sawInf, sawSum, sawCount bool
		for series := range samples {
			switch {
			case strings.HasPrefix(series, name+"_bucket") && strings.Contains(series, `le="+Inf"`):
				sawInf = true
			case strings.HasPrefix(series, name+"_sum"):
				sawSum = true
			case strings.HasPrefix(series, name+"_count"):
				sawCount = true
			}
		}
		if !sawInf || !sawSum || !sawCount {
			t.Errorf("histogram %s is incomplete: +Inf=%v sum=%v count=%v", name, sawInf, sawSum, sawCount)
		}
	}

	if len(families) == 0 {
		t.Error("scrape is empty")
	}
}

func parseSampleValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// validSeries checks a series' shape: a metric name, optionally followed by a
// brace-delimited list of name="value" pairs with every quote accounted for.
func validSeries(series string) bool {
	name, rest, hasLabels := strings.Cut(series, "{")
	if !validName(name) {
		return false
	}
	if !hasLabels {
		return true
	}
	if !strings.HasSuffix(rest, "}") {
		return false
	}
	body := strings.TrimSuffix(rest, "}")
	if body == "" {
		return false
	}

	// Walk the label block by hand rather than splitting on commas, which a
	// label value is free to contain.
	for {
		labelName, after, ok := strings.Cut(body, "=")
		if !ok || !validName(labelName) {
			return false
		}
		if !strings.HasPrefix(after, `"`) {
			return false
		}
		after = after[1:]
		// Find the closing quote, skipping escaped characters.
		i := 0
		for i < len(after) {
			if after[i] == '\\' {
				i += 2
				continue
			}
			if after[i] == '"' {
				break
			}
			i++
		}
		if i >= len(after) {
			return false
		}
		body = after[i+1:]
		if body == "" {
			return true
		}
		if body[0] != ',' {
			return false
		}
		body = body[1:]
	}
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func isSorted(items []string) bool {
	for i := 1; i < len(items); i++ {
		if items[i-1] > items[i] {
			return false
		}
	}
	return true
}

// bucketBoundaries returns the le values of a histogram family, in the order
// they were written.
func bucketBoundaries(t *testing.T, body, name string) []string {
	t.Helper()
	var les []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"_bucket{") {
			continue
		}
		series, _, _ := strings.Cut(line, " ")
		_, le, ok := strings.Cut(series, `le="`)
		if !ok {
			t.Fatalf("bucket line has no le label: %q", line)
		}
		le, _, _ = strings.Cut(le, `"`)
		les = append(les, le)
	}
	return les
}

// seriesValues indexes a scrape by series, for tests that care about numbers
// rather than layout.
func seriesValues(t *testing.T, body string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series, value, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("sample line has no value: %q", line)
		}
		f, err := parseSampleValue(value)
		if err != nil {
			t.Fatalf("sample %q has an unparseable value %q: %v", series, value, err)
		}
		out[series] = f
	}
	return out
}

func TestExponentialBucketsRejectsNonsense(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		start  float64
		factor float64
		count  int
	}{
		{"no buckets", 0.005, 2, 0},
		{"zero start", 0, 2, 4},
		{"factor of one", 0.005, 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			_ = metrics.ExponentialBuckets(tc.start, tc.factor, tc.count)
		})
	}
}

func TestWrongLabelCountPanics(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	c := r.NewCounterVec("things_total", "Things.", []string{"kind", "result"})

	defer func() {
		if recover() == nil {
			t.Error("expected a panic for the wrong number of label values")
		}
	}()
	c.WithLabelValues("only-one").Inc()
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	r.NewCounter("things_total", "Things.")

	defer func() {
		if recover() == nil {
			t.Error("expected a panic when a family name is reused")
		}
	}()
	r.NewGauge("things_total", "Also things.")
}

// TestBadNamesPanic covers the failure mode hand-rolling this most easily
// introduces: nothing between a registration and a scraper looks at a name, so
// an unusable one would be discovered by Prometheus rejecting the endpoint.
func TestBadNamesPanic(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		register func(r *metrics.Registry)
	}{
		{"dash in metric name", func(r *metrics.Registry) {
			r.NewCounter("things-total", "Things.")
		}},
		{"leading digit in metric name", func(r *metrics.Registry) {
			r.NewCounter("1things", "Things.")
		}},
		{"empty metric name", func(r *metrics.Registry) {
			r.NewCounter("", "Things.")
		}},
		{"space in metric name", func(r *metrics.Registry) {
			r.NewGauge("things total", "Things.")
		}},
		{"dash in label name", func(r *metrics.Registry) {
			r.NewCounterVec("things_total", "Things.", []string{"the-kind"})
		}},
		{"colon in label name", func(r *metrics.Registry) {
			r.NewCounterVec("things_total", "Things.", []string{"kind:sub"})
		}},
		{"reserved label name", func(r *metrics.Registry) {
			r.NewCounterVec("things_total", "Things.", []string{"__name__"})
		}},
		{"le on a histogram", func(r *metrics.Registry) {
			r.NewHistogramVec("dur_seconds", "How long.", []string{"le"}, []float64{1})
		}},
		{"duplicate label name", func(r *metrics.Registry) {
			r.NewCounterVec("things_total", "Things.", []string{"kind", "kind"})
		}},
		{"unsorted buckets", func(r *metrics.Registry) {
			r.NewHistogram("dur_seconds", "How long.", []float64{1, 0.5, 2})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			tc.register(metrics.NewRegistry())
		})
	}
}

// TestGoodNamesAreAccepted keeps the check above from being over-eager.
func TestGoodNamesAreAccepted(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	r.NewCounterVec("obsidibot_things_total", "Things.", []string{"kind", "result"}).
		WithLabelValues("a", "ok").Inc()
	r.NewCounter("_leading_underscore", "Odd but legal.").Inc()
	r.NewGauge("recording:rule:style", "A colon is legal in a metric name.").Set(1)

	validate(t, string(r.Gather()))
}

func TestCounterRejectsNegativeAdd(t *testing.T) {
	t.Parallel()

	r := metrics.NewRegistry()
	c := r.NewCounter("things_total", "Things.")

	defer func() {
		if recover() == nil {
			t.Error("expected a panic: counters only go up")
		}
	}()
	c.Add(-1)
}

func ExampleRegistry_Gather() {
	r := metrics.NewRegistry()
	r.NewCounterVec("greetings_total", "Greetings sent.", []string{"result"}).
		WithLabelValues("ok").Inc()
	fmt.Print(string(r.Gather()))
	// Output:
	// # HELP greetings_total Greetings sent.
	// # TYPE greetings_total counter
	// greetings_total{result="ok"} 1
}

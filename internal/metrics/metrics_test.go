package metrics

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// histogram is what the default registry reports for one histogram series.
type histogram struct {
	count  uint64
	sum    float64
	bounds []float64 // upper bounds of the finite buckets
	found  bool
}

func gatherHistogram(t *testing.T, name string, labels map[string]string) histogram {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, metric := range family.GetMetric() {
			have := map[string]string{}
			for _, label := range metric.GetLabel() {
				have[label.GetName()] = label.GetValue()
			}
			for k, v := range labels {
				if have[k] != v {
					continue metrics
				}
			}
			h := metric.GetHistogram()
			out := histogram{count: h.GetSampleCount(), sum: h.GetSampleSum(), found: true}
			for _, b := range h.GetBucket() {
				out.bounds = append(out.bounds, b.GetUpperBound())
			}
			return out
		}
	}
	return histogram{}
}

func phaseHistogram(t *testing.T, phase string) histogram {
	t.Helper()
	return gatherHistogram(t, "gateway_phase_"+phase+"_duration_seconds", nil)
}

func lookupCounts(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	counts := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "gateway_cache_lookups_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" {
					counts[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	return counts
}

// near reports whether two durations in seconds agree to well under a
// microsecond: a histogram's sum is a float.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestObserve(t *testing.T) {
	t.Run("records the elapsed time under its phase, when the returned func is called and not before", func(t *testing.T) {
		before := phaseHistogram(t, "jwt_verify")
		unrelated := phaseHistogram(t, "usage_parse")

		stop := Observe("jwt_verify")
		started := phaseHistogram(t, "jwt_verify")
		time.Sleep(5 * time.Millisecond)
		elapsed := stop()
		after := phaseHistogram(t, "jwt_verify")

		if started.count != before.count {
			t.Errorf("the phase was recorded as soon as it started: count %d -> %d", before.count, started.count)
		}
		if after.count != before.count+1 {
			t.Errorf("count = %d, want %d", after.count, before.count+1)
		}
		if elapsed < 5*time.Millisecond {
			t.Errorf("elapsed = %v, want at least the 5ms slept", elapsed)
		}
		if !near(after.sum-before.sum, elapsed.Seconds()) {
			t.Errorf("recorded %vs, want the returned %vs", after.sum-before.sum, elapsed.Seconds())
		}
		if got := phaseHistogram(t, "usage_parse"); got.count != unrelated.count {
			t.Errorf("another phase moved: count %d -> %d", unrelated.count, got.count)
		}
	})

	t.Run("a phase nobody registered goes to the fallback, labelled with its name", func(t *testing.T) {
		phase := fmt.Sprintf("test_phase_%d", time.Now().UnixNano())

		Observe(phase)()

		got := gatherHistogram(t, "gateway_phase_unregistered_duration_seconds", map[string]string{"phase": phase})
		if !got.found || got.count != 1 {
			t.Errorf("fallback series for %q = %+v, want one observation", phase, got)
		}
	})

	t.Run("a registered phase never reaches the fallback", func(t *testing.T) {
		Observe("jwt_verify")()

		got := gatherHistogram(t, "gateway_phase_unregistered_duration_seconds", map[string]string{"phase": "jwt_verify"})
		if got.found {
			t.Errorf("jwt_verify showed up in the fallback: %+v", got)
		}
	})
}

func TestObserveSince(t *testing.T) {
	before := phaseHistogram(t, "usage_parse")

	elapsed := ObserveSince("usage_parse", time.Now().Add(-2*time.Second))

	after := phaseHistogram(t, "usage_parse")
	if elapsed < 2*time.Second {
		t.Errorf("elapsed = %v, want at least the 2s since start", elapsed)
	}
	if after.count != before.count+1 {
		t.Errorf("count = %d, want %d", after.count, before.count+1)
	}
	if !near(after.sum-before.sum, elapsed.Seconds()) {
		t.Errorf("recorded %vs, want the returned %vs", after.sum-before.sum, elapsed.Seconds())
	}
}

func TestObserveDuration(t *testing.T) {
	before := phaseHistogram(t, "usage_parse_stream")

	ObserveDuration("usage_parse_stream", 250*time.Millisecond)

	after := phaseHistogram(t, "usage_parse_stream")
	if after.count != before.count+1 {
		t.Errorf("count = %d, want %d", after.count, before.count+1)
	}
	if !near(after.sum-before.sum, 0.25) {
		t.Errorf("recorded %vs, want 0.25s", after.sum-before.sum)
	}
}

func TestCacheCounters(t *testing.T) {
	tests := []struct {
		result string
		record func()
	}{
		{"local_hit", CacheLocalHit},
		{"hit", CacheHit},
		{"miss", CacheMiss},
	}
	for _, tt := range tests {
		t.Run(tt.result, func(t *testing.T) {
			before := lookupCounts(t)

			tt.record()

			after := lookupCounts(t)
			for _, result := range []string{"local_hit", "hit", "miss"} {
				want := 0.0
				if result == tt.result {
					want = 1
				}
				if got := after[result] - before[result]; got != want {
					t.Errorf("%s rose by %v, want %v", result, got, want)
				}
			}
		})
	}
}

func TestPhaseHistograms(t *testing.T) {
	t.Run("every phase is exposed as gateway_phase_<phase>_duration_seconds", func(t *testing.T) {
		for phase := range phaseObservers {
			if !phaseHistogram(t, phase).found {
				t.Errorf("no metric gateway_phase_%s_duration_seconds is registered", phase)
			}
		}
	})

	// A bucket layout has to cover what its phases really take: a fine
	// phase that stopped at 100ms showed p99 capped there under load.
	t.Run("each group of phases has buckets reaching down and up far enough", func(t *testing.T) {
		groups := []struct {
			name   string
			phases []string
			bottom float64 // the smallest bucket must be at most this
			top    float64 // the largest finite bucket must be at least this
		}{
			{
				"in-process phases, reported by quantile over a chosen window",
				[]string{
					"time_to_forward_excl_kms", "handle_cache", "read_payload_scan",
					"handle_cache_redis_get", "handle_cache_decode_entry", "handle_cache_redis_set",
				},
				0.0005, 0.3,
			},
			{
				"phases that include a database call or a client upload, or aren't profiled yet",
				[]string{
					"read_payload_body", "handle_cache_db_lookup", "time_to_forward",
					"usage_parse", "usage_parse_stream", "jwt_verify",
				},
				0.0005, 16,
			},
			{
				"network round trips to a provider or a KMS",
				[]string{"provider_roundtrip", "kms_decrypt"},
				0.005, 10,
			},
		}
		covered := 0
		for _, g := range groups {
			for _, phase := range g.phases {
				covered++
				bounds := phaseHistogram(t, phase).bounds
				if len(bounds) == 0 {
					t.Errorf("%s (%s): no buckets", phase, g.name)
					continue
				}
				if bounds[0] > g.bottom+1e-9 {
					t.Errorf("%s: smallest bucket is %vs, want at most %vs (%s)", phase, bounds[0], g.bottom, g.name)
				}
				if top := bounds[len(bounds)-1]; top < g.top-1e-9 {
					t.Errorf("%s: largest bucket is %vs, want at least %vs (%s)", phase, top, g.top, g.name)
				}
			}
		}
		if covered != len(phaseObservers) {
			t.Errorf("the groups cover %d phases, but %d are registered: put the new one in a group", covered, len(phaseObservers))
		}
	})
}

// A phase used in the code but missing from phaseObservers is reported under
// the "unregistered" histogram instead of its own, and nothing says so.
func TestEveryPhaseInTheCodeIsRegistered(t *testing.T) {
	call := regexp.MustCompile(`metrics\.(?:Observe|ObserveSince|ObserveDuration)\(\s*"([^"]+)"`)
	root := filepath.Join("..", "..")

	used := map[string]string{} // phase -> a file that uses it
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range call.FindAllStringSubmatch(string(source), -1) {
			used[match[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the source: %v", err)
	}

	if len(used) == 0 {
		t.Fatal("found no metrics.Observe calls, so this check isn't looking at anything")
	}
	for phase, file := range used {
		if _, ok := phaseObservers[phase]; !ok {
			t.Errorf("phase %q, used in %s, isn't in phaseObservers", phase, filepath.ToSlash(file))
		}
	}
}

// Package metrics exposes internal timing breakdowns (cache lookups, KMS
// encrypt/decrypt, provider round trips, usage parsing, ...) as Prometheus
// metrics, so per-phase latency can be inspected without reading logs.
//
// Each phase gets its own separately-named metric instead of one shared
// histogram with a "phase" label: phases span wildly different scales (a
// few ms for time_to_forward_excl_kms vs. hundreds of ms to seconds for
// provider_roundtrip), and a Prometheus histogram has one fixed bucket
// layout per metric — one layout can't serve both well, and a Histogram
// and a Summary can't share a name at all. Metric names are
// gateway_phase_<phase>_duration_seconds.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// highLatencyBuckets is for phases that leave the process — a real network
// round trip to an LLM provider, or to a KMS that isn't the in-process
// LocalKMS — reasonably spanning a few ms up to several seconds under load.
var highLatencyBuckets = prometheus.ExponentialBuckets(0.005, 1.5, 20) // 5ms .. ~11s

// defaultBuckets is for any phase below that hasn't been profiled under
// load — still far tighter than prometheus.DefBuckets' 5ms floor, so a new
// phase doesn't quietly inherit the same all-in-one-bucket problem this
// file exists to avoid.
var defaultBuckets = prometheus.ExponentialBuckets(0.0005, 2, 16) // 0.5ms .. ~16s

// fineBuckets is for low-latency phases (well under prometheus.DefBuckets'
// 5ms floor) that need to report p95/median/etc for a specific, chosen time
// window — e.g. via histogram_quantile(..., [$__range]) — rather than a
// fixed lookback. 0.5ms linear steps up to 100ms, where these phases
// normally sit, then 5ms steps up to 300ms so slow outliers under load
// still land in a real bucket instead of +Inf. Coarser above 100ms because
// every bucket is its own time series, and that precision isn't needed there.
var fineBuckets = append(
	prometheus.LinearBuckets(0.0005, 0.0005, 200), // 0.5ms .. 100ms
	prometheus.LinearBuckets(0.105, 0.005, 40)..., // 105ms .. 300ms
)

func newHistogram(phase string, buckets []float64) prometheus.Histogram {
	return prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_phase_" + phase + "_duration_seconds",
		Help:    "Duration of the " + phase + " phase during request handling",
		Buckets: buckets,
	})
}

var phaseObservers = map[string]prometheus.Observer{
	// Fine-bucketed rather than the default/exponential buckets below: both
	// consistently sub-10ms, and reported (median/p95/p99/avg) for specific,
	// chosen time windows via histogram_quantile, which needs query-time
	// flexibility a Summary's fixed internal window can't give.
	"time_to_forward_excl_kms": newHistogram("time_to_forward_excl_kms", fineBuckets),
	"handle_cache":             newHistogram("handle_cache", fineBuckets),

	// Reading the payload, before handle_cache. The body read gets default
	// buckets: it includes receiving the body from the client, so it can run
	// past fineBuckets' 100ms cap.
	"read_payload_body": newHistogram("read_payload_body", defaultBuckets),
	"read_payload_scan": newHistogram("read_payload_scan", fineBuckets),

	// handle_cache sub-phases. The DB lookup gets default buckets: it's a
	// Postgres round trip, so it can run past fineBuckets' 100ms cap.
	"handle_cache_redis_get":    newHistogram("handle_cache_redis_get", fineBuckets),
	"handle_cache_decode_entry": newHistogram("handle_cache_decode_entry", fineBuckets),
	"handle_cache_db_lookup":    newHistogram("handle_cache_db_lookup", defaultBuckets),
	"handle_cache_redis_set":    newHistogram("handle_cache_redis_set", fineBuckets),

	// Network round trips: wide range, tuned buckets.
	"provider_roundtrip": newHistogram("provider_roundtrip", highLatencyBuckets),
	"kms_decrypt":        newHistogram("kms_decrypt", highLatencyBuckets),

	// Not yet profiled under load — default buckets rather than
	// prometheus.DefBuckets, so they're not silently exposed to the same
	// precision problem later.
	"time_to_forward":    newHistogram("time_to_forward", defaultBuckets),
	"usage_parse":        newHistogram("usage_parse", defaultBuckets),
	"usage_parse_stream": newHistogram("usage_parse_stream", defaultBuckets),
	"jwt_verify":         newHistogram("jwt_verify", defaultBuckets),
}

// fallback catches any phase not pre-registered in phaseObservers (e.g. a
// new call site added without updating this file) — prometheus.DefBuckets,
// matching the old blanket behavior, rather than panicking or silently
// dropping the observation. If this metric ever gets populated, it means
// phaseObservers is missing an entry for whatever "phase" label shows up.
var fallback = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "gateway_phase_unregistered_duration_seconds",
		Help:    "Duration of a phase not explicitly registered in internal/metrics",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"phase"},
)

// cacheLookups counts handle_cache's session lookups by result: local_hit
// (served from the in-process layer), hit (from Redis) or miss (fell
// through to Postgres). Redis's own keyspace_hits/misses can't stand in for
// this: asynq's Lua scripts run EXISTS/GET on the same instance and are
// counted there too.
var cacheLookups = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gateway_cache_lookups_total",
		Help: "Session cache lookups in handle_cache, by result (local_hit, hit or miss)",
	},
	[]string{"result"},
)

var (
	cacheLocalHits = cacheLookups.WithLabelValues("local_hit")
	cacheHits      = cacheLookups.WithLabelValues("hit")
	cacheMisses    = cacheLookups.WithLabelValues("miss")
)

func init() {
	for _, o := range phaseObservers {
		prometheus.MustRegister(o.(prometheus.Collector))
	}
	prometheus.MustRegister(fallback)
	prometheus.MustRegister(cacheLookups)
}

// CacheLocalHit records a session served from handle_cache's in-process layer.
func CacheLocalHit() { cacheLocalHits.Inc() }

// CacheHit records a session cache hit in Redis in handle_cache.
func CacheHit() { cacheHits.Inc() }

// CacheMiss records a session cache miss in handle_cache.
func CacheMiss() { cacheMisses.Inc() }

func observerFor(phase string) prometheus.Observer {
	if o, ok := phaseObservers[phase]; ok {
		return o
	}
	return fallback.WithLabelValues(phase)
}

// Observe starts a timer for phase and returns a func that records the
// elapsed duration when called and also returns it — most callers just
// discard the return value:
//
//	defer metrics.Observe("kms_decrypt")()
//
// Callers that need the raw value (e.g. to derive a further phase by
// subtracting it from another) can capture it instead:
//
//	stop := metrics.Observe("kms_decrypt")
//	...
//	kmsDecrypt := stop()
func Observe(phase string) func() time.Duration {
	start := time.Now()
	observer := observerFor(phase)
	return func() time.Duration {
		elapsed := time.Since(start)
		observer.Observe(elapsed.Seconds())
		return elapsed
	}
}

// ObserveSince records the elapsed time since start under phase and returns
// it — for when the starting instant was captured earlier (e.g. by another
// package) rather than right where the phase is being recorded.
func ObserveSince(phase string, start time.Time) time.Duration {
	elapsed := time.Since(start)
	observerFor(phase).Observe(elapsed.Seconds())
	return elapsed
}

// ObserveDuration records a duration computed elsewhere directly under
// phase — e.g. one phase's total minus a sub-phase already observed within
// it, to isolate the remaining, otherwise-unaccounted-for time.
//
// Histograms can't be validly subtracted from each other after the fact
// (percentiles aren't linear, so histogram_quantile(a) - histogram_quantile(b)
// isn't the true quantile of the per-request difference) — so derived
// phases like this need to be computed here, per request, and observed as
// their own phase, rather than reconstructed later in Grafana/PromQL.
func ObserveDuration(phase string, d time.Duration) {
	observerFor(phase).Observe(d.Seconds())
}

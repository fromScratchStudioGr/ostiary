package usage

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"

	"github.com/andybalholm/brotli"
	"github.com/hibiken/asynq"
)

func TestToUsageRecordOptionalTiers(t *testing.T) {
	usage := &UsageInfo{
		CompletionTokens:             1_000_000,
		PromptCacheHitTokens:         1_000_000,
		PromptCacheMissTokens:        1_000_000,
		PromptCacheWriteLevel1Tokens: 1_000_000,
		PromptCacheWriteLevel2Tokens: 1_000_000,
		PriceMultiplier:              1,
	}

	t.Run("priced tiers are billed", func(t *testing.T) {
		model := providers.Model{Cost: providers.Cost{
			Input: 1, Output: 2,
			CacheRead:        providers.Price(0.5),
			CacheWriteLevel1: providers.Price(3),
			CacheWriteLevel2: providers.Price(4),
		}}
		got := toUsageRecord(model, 1, 1, usage, time.Time{})
		if got.PromptCacheHitTokensCost != 0.5 || got.PromptCacheWriteLevel1TokensCost != 3 || got.PromptCacheWriteLevel2TokensCost != 4 {
			t.Fatalf("unexpected tier costs: %+v", got)
		}
	})

	t.Run("missing tiers bill 0 but keep the tokens", func(t *testing.T) {
		model := providers.Model{Cost: providers.Cost{Input: 1, Output: 2, CacheRead: providers.Price(0.5)}}
		got := toUsageRecord(model, 1, 1, usage, time.Time{})
		if got.PromptCacheWriteLevel1TokensCost != 0 || got.PromptCacheWriteLevel2TokensCost != 0 {
			t.Fatalf("unpriced tiers should cost 0: %+v", got)
		}
		if got.PromptCacheWriteLevel1Tokens != 1_000_000 || got.PromptCacheWriteLevel2Tokens != 1_000_000 {
			t.Fatalf("tokens for unpriced tiers should still be recorded: %+v", got)
		}
		if math.Abs(got.CompletionTokensCost-2) > 1e-9 || math.Abs(got.PromptCacheMissTokensCost-1) > 1e-9 {
			t.Fatalf("required tiers should still be billed: %+v", got)
		}
	})

	t.Run("an explicit zero price is a priced tier", func(t *testing.T) {
		model := providers.Model{Cost: providers.Cost{Input: 1, Output: 2, CacheRead: providers.Price(0)}}
		if got := toUsageRecord(model, 1, 1, usage, time.Time{}); got.PromptCacheHitTokensCost != 0 {
			t.Fatalf("got %v, want 0", got.PromptCacheHitTokensCost)
		}
	})
}

func TestToUsageRecordPeakPricing(t *testing.T) {
	usage := &UsageInfo{
		CompletionTokens:      1_000_000,
		PromptCacheHitTokens:  1_000_000,
		PromptCacheMissTokens: 1_000_000,
		PriceMultiplier:       1.1,
	}
	peak := &providers.PeakPricing{
		Multiplier:  2,
		Weekdays:    []time.Weekday{time.Monday},
		HourWindows: []providers.HourWindow{{Start: 1, End: 4}},
	}
	model := providers.Model{
		PeakPricing: peak,
		Cost:        providers.Cost{Input: 1, Output: 2, CacheRead: providers.Price(0.5)},
	}

	peakTime := time.Date(2026, 1, 5, 2, 0, 0, 0, time.UTC)
	offPeakTime := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	atPeak := toUsageRecord(model, 1, 1, usage, peakTime)
	offPeak := toUsageRecord(model, 1, 1, usage, offPeakTime)

	// The peak multiplier stacks with the usage's own (e.g. regional) one, and
	// applies to every tier.
	pairs := []struct {
		name          string
		peak, offPeak float64
	}{
		{"output", atPeak.CompletionTokensCost, offPeak.CompletionTokensCost},
		{"input", atPeak.PromptCacheMissTokensCost, offPeak.PromptCacheMissTokensCost},
		{"cache read", atPeak.PromptCacheHitTokensCost, offPeak.PromptCacheHitTokensCost},
	}
	for _, p := range pairs {
		if p.offPeak == 0 || math.Abs(p.peak/p.offPeak-2) > 1e-9 {
			t.Errorf("%s: peak %v vs off-peak %v, want a ratio of 2", p.name, p.peak, p.offPeak)
		}
	}
	if math.Abs(offPeak.CompletionTokensCost-2.2) > 1e-9 {
		t.Errorf("off-peak output cost = %v, want 2.2 (only the 1.1 usage multiplier)", offPeak.CompletionTokensCost)
	}

	// A model without peak pricing is unaffected by the time.
	plain := providers.Model{Cost: model.Cost}
	a, b := toUsageRecord(plain, 1, 1, usage, peakTime), toUsageRecord(plain, 1, 1, usage, offPeakTime)
	if a.CompletionTokensCost != b.CompletionTokensCost || a.PromptCacheMissTokensCost != b.PromptCacheMissTokensCost || a.PromptCacheHitTokensCost != b.PromptCacheHitTokensCost {
		t.Errorf("model without peak pricing priced differently by time: %+v vs %+v", a, b)
	}
}

func TestToUsageRecordLongContext(t *testing.T) {
	model := providers.Model{Cost: providers.Cost{
		Input: 1, Output: 2,
		CacheRead:        providers.Price(0.1),
		CacheWriteLevel1: providers.Price(1.25),
		LongContext: &providers.LongContextCost{
			ThresholdTokens: 1000,
			Cost: providers.Cost{
				Input: 2, Output: 3,
				CacheRead:        providers.Price(0.2),
				CacheWriteLevel1: providers.Price(2.5),
			},
		},
	}}

	// 1M tokens per output bucket, spread over the input buckets so the
	// total is exactly inputTokens.
	record := func(hit, miss, write int) models.Usage {
		return toUsageRecord(model, 1, 1, &UsageInfo{
			CompletionTokens:             1_000_000,
			PromptCacheHitTokens:         hit,
			PromptCacheMissTokens:        miss,
			PromptCacheWriteLevel1Tokens: write,
			PriceMultiplier:              1,
		}, time.Time{})
	}

	t.Run("at the threshold is still short context", func(t *testing.T) {
		got := record(300, 400, 300)
		if got.CompletionTokensCost != 2 {
			t.Fatalf("output cost = %v, want 2 (short-context rate)", got.CompletionTokensCost)
		}
	})

	t.Run("over the threshold reprices the whole request", func(t *testing.T) {
		got := record(300, 400, 301)
		if got.CompletionTokensCost != 3 {
			t.Fatalf("output cost = %v, want 3 (long-context rate)", got.CompletionTokensCost)
		}
		if want := 300.0 / 1_000_000 * 0.2; math.Abs(got.PromptCacheHitTokensCost-want) > 1e-12 {
			t.Fatalf("cache read cost = %v, want %v", got.PromptCacheHitTokensCost, want)
		}
		if want := 400.0 / 1_000_000 * 2; math.Abs(got.PromptCacheMissTokensCost-want) > 1e-12 {
			t.Fatalf("input cost = %v, want %v", got.PromptCacheMissTokensCost, want)
		}
		if want := 301.0 / 1_000_000 * 2.5; math.Abs(got.PromptCacheWriteLevel1TokensCost-want) > 1e-12 {
			t.Fatalf("cache write cost = %v, want %v", got.PromptCacheWriteLevel1TokensCost, want)
		}
	})

	t.Run("a model without a long-context tier is unaffected by size", func(t *testing.T) {
		plain := providers.Model{Cost: providers.Cost{Input: 1, Output: 2}}
		got := toUsageRecord(plain, 1, 1, &UsageInfo{CompletionTokens: 1_000_000, PromptCacheMissTokens: 5_000_000, PriceMultiplier: 1}, time.Time{})
		if got.CompletionTokensCost != 2 || got.PromptCacheMissTokensCost != 5 {
			t.Fatalf("unexpected costs: %+v", got)
		}
	})
}

func TestTierCost(t *testing.T) {
	tests := []struct {
		name       string
		tokens     int
		price      *float32
		multiplier float64
		want       float64
	}{
		{name: "a million tokens cost the price per million", tokens: 1_000_000, price: providers.Price(3), multiplier: 1, want: 3},
		{name: "cost scales linearly with the tokens", tokens: 250_000, price: providers.Price(2), multiplier: 1, want: 0.5},
		{name: "the multiplier applies on top", tokens: 1_000_000, price: providers.Price(2), multiplier: 1.5, want: 3},
		{name: "no tokens cost nothing", tokens: 0, price: providers.Price(3), multiplier: 1, want: 0},
		{name: "an explicit zero price costs nothing", tokens: 1_000_000, price: providers.Price(0), multiplier: 1, want: 0},
		{name: "a missing price bills 0 whatever the tokens", tokens: 1_000_000, price: nil, multiplier: 1.5, want: 0},
	}

	model := providers.Model{Id: "test-model"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tierCost(model, "cache_read", tt.tokens, tt.price, tt.multiplier)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A tier the model has no price for bills 0, which would silently under-bill
// if nobody noticed, so tierCost logs a warning. It must do so only when
// there are tokens to bill: a response that simply doesn't use the tier is
// normal, and warning on it would bury the real signal.
func TestTierCostWarnsAboutAMissingPrice(t *testing.T) {
	tests := []struct {
		name     string
		tokens   int
		price    *float32
		wantWarn bool
	}{
		{name: "tokens without a price warn", tokens: 42, price: nil, wantWarn: true},
		{name: "no tokens and no price stay quiet", tokens: 0, price: nil, wantWarn: false},
		{name: "a priced tier stays quiet", tokens: 42, price: providers.Price(3), wantWarn: false},
		{name: "an explicit zero price is priced, so it stays quiet", tokens: 42, price: providers.Price(0), wantWarn: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)

			tierCost(providers.Model{Id: "test-model"}, "cache_read", tt.tokens, tt.price, 1)

			if !tt.wantWarn {
				if logs.Len() != 0 {
					t.Fatalf("unexpected log output: %s", logs.String())
				}
				return
			}
			// The warning has to say which model, tier and token count so a
			// missing price can actually be found and fixed.
			for _, want := range []string{"level=WARN", "model=test-model", "tier=cache_read", "tokens=42"} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("log output %q is missing %q", logs.String(), want)
				}
			}
		})
	}
}

// captureLogs sends slog output to the returned buffer until the test ends.
// It swaps the process-wide default logger, so tests using it can't run in
// parallel with each other.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// logUsageFixture is a persistFixture plus what LogUsage needs on top: an
// asynq client and inspector on the same Redis, a priced model, and a usage
// to log. Every token bucket holds a million tokens, so each cost equals its
// per-million price (the five add up to 10.5).
type logUsageFixture struct {
	*persistFixture
	client    *asynq.Client
	inspector *asynq.Inspector
	model     providers.Model
	usage     *UsageInfo
}

func newLogUsageFixture(t *testing.T) *logUsageFixture {
	t.Helper()
	f := &logUsageFixture{persistFixture: newPersistFixture(t, true, 100, 0)}

	opt := asynq.RedisClientOpt{Addr: f.rdb.Options().Addr}
	f.client = asynq.NewClient(opt)
	t.Cleanup(func() { f.client.Close() }) // tests close it themselves to make enqueues fail
	f.inspector = asynq.NewInspector(opt)

	// asynq's default queue is shared by every test, and tasks outlive the
	// test that enqueued them, so empty it before and after.
	f.clearQueue()
	t.Cleanup(func() {
		f.clearQueue()
		f.inspector.Close()
	})

	f.model = providers.Model{Id: "test-model", Cost: providers.Cost{
		Input: 1, Output: 2,
		CacheRead:        providers.Price(0.5),
		CacheWriteLevel1: providers.Price(3),
		CacheWriteLevel2: providers.Price(4),
	}}
	f.usage = &UsageInfo{
		CompletionTokens:             1_000_000,
		PromptCacheHitTokens:         1_000_000,
		PromptCacheMissTokens:        1_000_000,
		PromptCacheWriteLevel1Tokens: 1_000_000,
		PromptCacheWriteLevel2Tokens: 1_000_000,
		PriceMultiplier:              1,
	}
	return f
}

func (f *logUsageFixture) clearQueue() {
	f.inspector.DeleteAllPendingTasks("default")
}

func (f *logUsageFixture) pending(t *testing.T) []*asynq.TaskInfo {
	t.Helper()
	tasks, err := f.inspector.ListPendingTasks("default")
	if errors.Is(err, asynq.ErrQueueNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("listing queued tasks: %v", err)
	}
	return tasks
}

func (f *logUsageFixture) logUsage(ctx context.Context) {
	LogUsage(ctx, f.client, f.db, f.cache, f.model, f.consumer.ID, f.provider.ID, f.usage, time.Time{}, 7)
}

// requireStored checks that exactly one usage row exists and that it has the
// priced tokens and costs the fixture's usage works out to.
func (f *logUsageFixture) requireStored(t *testing.T) {
	t.Helper()
	want := toUsageRecord(f.model, f.consumer.ID, f.provider.ID, f.usage, time.Time{})

	if rows := f.usageRows(t); rows != 1 {
		t.Fatalf("usage rows = %d, want 1", rows)
	}
	var got models.Usage
	if err := f.db.Where("consumer_id = ?", f.consumer.ID).First(&got).Error; err != nil {
		t.Fatalf("loading the usage row: %v", err)
	}
	if got.CompletionTokens != want.CompletionTokens || got.PromptCacheHitTokens != want.PromptCacheHitTokens ||
		got.PromptCacheMissTokens != want.PromptCacheMissTokens ||
		got.PromptCacheWriteLevel1Tokens != want.PromptCacheWriteLevel1Tokens ||
		got.PromptCacheWriteLevel2Tokens != want.PromptCacheWriteLevel2Tokens {
		t.Fatalf("stored tokens %+v, want %+v", got, want)
	}
	if !approxEqual(got.CompletionTokensCost, 2) || !approxEqual(got.PromptCacheHitTokensCost, 0.5) ||
		!approxEqual(got.PromptCacheMissTokensCost, 1) || !approxEqual(got.PromptCacheWriteLevel1TokensCost, 3) ||
		!approxEqual(got.PromptCacheWriteLevel2TokensCost, 4) {
		t.Fatalf("stored costs %+v, want 2 / 0.5 / 1 / 3 / 4", got)
	}
	if total := f.access(t).TotalCost; !approxEqual(total, 10.5) {
		t.Fatalf("total cost = %v, want 10.5", total)
	}
}

func TestLogUsage(t *testing.T) {
	t.Run("does nothing when there is no usage", func(t *testing.T) {
		f := newLogUsageFixture(t)
		f.usage = nil

		f.logUsage(f.ctx)

		if tasks := f.pending(t); len(tasks) != 0 {
			t.Fatalf("%d tasks queued, want none", len(tasks))
		}
		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0", rows)
		}
	})

	t.Run("queues the priced record and doesn't also write it directly", func(t *testing.T) {
		f := newLogUsageFixture(t)

		f.logUsage(f.ctx)

		tasks := f.pending(t)
		if len(tasks) != 1 || tasks[0].Type != TaskTypeLogUsage {
			t.Fatalf("queued tasks = %+v, want exactly one %q", tasks, TaskTypeLogUsage)
		}
		if tasks[0].MaxRetry != 7 {
			t.Errorf("the task is retried up to %d times, want the 7 LogUsage was given", tasks[0].MaxRetry)
		}
		// A direct write on top of the queued one would bill the request twice.
		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d right after queueing, want 0 (the worker writes it later)", rows)
		}

		// What was queued is the priced record: processing it stores the
		// same row the direct-write path below does.
		task := asynq.NewTask(tasks[0].Type, tasks[0].Payload)
		if err := HandleLogUsageTask(f.db, f.cache)(f.ctx, task); err != nil {
			t.Fatalf("handler: %v", err)
		}
		f.requireStored(t)
	})

	t.Run("falls back to a direct write when the enqueue fails", func(t *testing.T) {
		f := newLogUsageFixture(t)
		logs := captureLogs(t)
		f.client.Close() // from here on every Enqueue fails

		f.logUsage(f.ctx)

		f.requireStored(t)
		if tasks := f.pending(t); len(tasks) != 0 {
			t.Fatalf("%d tasks queued, want none (a record in both places would bill twice)", len(tasks))
		}
		if !strings.Contains(logs.String(), "could not enqueue usage task, falling back to direct write") {
			t.Fatalf("the fallback must be logged, got: %s", logs.String())
		}
	})

	t.Run("falls back to a direct write when the task can't be built", func(t *testing.T) {
		f := newLogUsageFixture(t)
		logs := captureLogs(t)
		// A NaN price makes the cost NaN, which JSON can't encode, so the
		// task can't be built.
		f.model.Cost.Output = float32(math.NaN())

		f.logUsage(f.ctx)

		if tasks := f.pending(t); len(tasks) != 0 {
			t.Fatalf("%d tasks queued, want none", len(tasks))
		}
		if rows := f.usageRows(t); rows != 1 {
			t.Fatalf("usage rows = %d, want 1 (the record must not be lost)", rows)
		}
		var got models.Usage
		if err := f.db.Where("consumer_id = ?", f.consumer.ID).First(&got).Error; err != nil {
			t.Fatalf("loading the usage row: %v", err)
		}
		if got.CompletionTokens != 1_000_000 {
			t.Fatalf("completion tokens = %d, want the usage's 1000000", got.CompletionTokens)
		}
		if !strings.Contains(logs.String(), "could not build usage task, falling back to direct write") {
			t.Fatalf("the fallback must be logged, got: %s", logs.String())
		}
	})

	t.Run("the fallback still stores the record after the request's context is cancelled", func(t *testing.T) {
		f := newLogUsageFixture(t)
		f.client.Close()

		// By the time the proxied response is done (or the client has
		// disconnected) the request's context is cancelled, and that mustn't
		// cost the billing record.
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()

		f.logUsage(ctx)

		f.requireStored(t)
	})

	t.Run("logs and gives up quietly when both the enqueue and the direct write fail", func(t *testing.T) {
		f := newLogUsageFixture(t)
		logs := captureLogs(t)
		f.client.Close()
		if err := f.db.Exec("DROP TABLE user_provider_accesses").Error; err != nil {
			t.Fatalf("dropping table: %v", err)
		}

		f.logUsage(f.ctx) // must not panic

		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0 (the failed write rolls back)", rows)
		}
		if !strings.Contains(logs.String(), "could not store usage (both enqueue and direct write failed)") {
			t.Fatalf("losing the record must be logged, got: %s", logs.String())
		}
	})
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func brotliBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write(data); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeContentEncoding(t *testing.T) {
	small := []byte("data: {\"usage\":{\"completion_tokens\":20}}\n\ndata: [DONE]\n\n")
	// Large enough to cross the decompressors' internal buffers, so a decoder
	// that only reads the first chunk would come back short.
	large := bytes.Repeat(small, 5000)

	tests := []struct {
		name     string
		encoding string
		wire     []byte // what the provider sent
		want     []byte // what the usage parser should get
	}{
		{name: "gzip", encoding: "gzip", wire: gzipBytes(t, small), want: small},
		{name: "gzip, large", encoding: "gzip", wire: gzipBytes(t, large), want: large},
		{name: "brotli", encoding: "br", wire: brotliBytes(t, small), want: small},
		{name: "brotli, large", encoding: "br", wire: brotliBytes(t, large), want: large},

		// Content codings are case-insensitive (RFC 9110), so these mean
		// exactly the same as the lowercase names above.
		{name: "gzip in upper case", encoding: "GZIP", wire: gzipBytes(t, small), want: small},
		{name: "gzip in mixed case", encoding: "Gzip", wire: gzipBytes(t, small), want: small},
		{name: "brotli in upper case", encoding: "BR", wire: brotliBytes(t, small), want: small},
		{name: "brotli in mixed case", encoding: "Br", wire: brotliBytes(t, small), want: small},

		// Anything else is handed back untouched. That's right for a body
		// the provider didn't compress, but it means a compression this
		// package can't decode reaches the usage parser still compressed.
		{name: "no encoding", encoding: "", wire: small, want: small},
		{name: "identity", encoding: "identity", wire: small, want: small},
		{name: "unsupported deflate", encoding: "deflate", wire: small, want: small},
		{name: "unsupported zstd", encoding: "zstd", wire: small, want: small},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeContentEncoding(tt.encoding, tt.wire)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("decoded %d bytes, want %d (content differs)", len(got), len(tt.want))
			}
		})
	}
}

// A body that doesn't match its Content-Encoding must be reported, not
// passed along as if it were decoded: the caller logs the error and skips the
// usage parse, instead of parsing garbage.
func TestDecodeContentEncodingRejectsCorruptInput(t *testing.T) {
	payload := bytes.Repeat([]byte("data: {\"usage\":{\"completion_tokens\":20}}\n\n"), 5000)
	gzipped := gzipBytes(t, payload)
	brotlied := brotliBytes(t, payload)

	tests := []struct {
		name     string
		encoding string
		wire     []byte
	}{
		{name: "gzip: not gzip data", encoding: "gzip", wire: []byte("plain text, not gzip")},
		{name: "gzip: cut off mid-stream", encoding: "gzip", wire: gzipped[:len(gzipped)/2]},
		{name: "gzip: empty body", encoding: "gzip", wire: nil},
		{name: "brotli: not brotli data", encoding: "br", wire: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{name: "brotli: cut off mid-stream", encoding: "br", wire: brotlied[:len(brotlied)/2]},
		{name: "brotli: empty body", encoding: "br", wire: nil},

		// Upper case still means gzip/brotli: corrupt data under it must be
		// an error, not silently passed along as if it were something else.
		{name: "gzip in upper case: not gzip data", encoding: "GZIP", wire: []byte("plain text, not gzip")},
		{name: "brotli in upper case: not brotli data", encoding: "BR", wire: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeContentEncoding(tt.encoding, tt.wire); err == nil {
				t.Fatal("got nil error, want one")
			}
		})
	}
}

// trackingBody is a response body whose Close can be observed and made to fail.
type trackingBody struct {
	io.Reader
	closeErr error
	onClose  func()
}

func (b *trackingBody) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	return b.closeErr
}

func TestUsageTeeBody(t *testing.T) {
	// teeOver wraps a reader in a UsageTeeBody whose OnClose records a copy of
	// what it was given (the buffer it receives is only valid until the next
	// write).
	teeOver := func(r io.Reader) (*UsageTeeBody, *[][]byte) {
		var calls [][]byte
		tee := &UsageTeeBody{
			ReadCloser: io.NopCloser(r),
			OnClose:    func(data []byte) { calls = append(calls, append([]byte(nil), data...)) },
		}
		return tee, &calls
	}

	payload := bytes.Repeat([]byte("data: {\"usage\":{\"completion_tokens\":20}}\n\n"), 3000)

	t.Run("passes every byte through unchanged and hands them all to OnClose on Close", func(t *testing.T) {
		tee, calls := teeOver(bytes.NewReader(payload))

		got, err := io.ReadAll(tee)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("the client got %d bytes, want the provider's %d unchanged", len(got), len(payload))
		}
		if len(*calls) != 0 {
			t.Fatal("OnClose ran before Close")
		}

		if err := tee.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if len(*calls) != 1 || !bytes.Equal((*calls)[0], payload) {
			t.Fatalf("OnClose calls = %d, want exactly one with all %d bytes", len(*calls), len(payload))
		}
	})

	t.Run("records a stream delivered one byte at a time", func(t *testing.T) {
		tee, calls := teeOver(iotest.OneByteReader(bytes.NewReader(payload[:2000])))

		got, err := io.ReadAll(tee)
		if err != nil || !bytes.Equal(got, payload[:2000]) {
			t.Fatalf("read returned %d bytes, err %v", len(got), err)
		}
		tee.Close()

		if len(*calls) != 1 || !bytes.Equal((*calls)[0], payload[:2000]) {
			t.Fatal("OnClose didn't get every byte")
		}
	})

	t.Run("keeps the last bytes when they arrive together with io.EOF", func(t *testing.T) {
		// io.Reader allows returning data and an error from the same call.
		// Usage arrives in the final chunk, so dropping it would lose the bill.
		tee, calls := teeOver(iotest.DataErrReader(bytes.NewReader(payload)))

		got, err := io.ReadAll(tee)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("read returned %d bytes, err %v", len(got), err)
		}
		tee.Close()

		if len(*calls) != 1 || !bytes.Equal((*calls)[0], payload) {
			t.Fatal("OnClose didn't get the final bytes")
		}
	})

	t.Run("returns a mid-stream error unchanged and still records what arrived before it", func(t *testing.T) {
		errBoom := errors.New("connection reset by peer")
		tee, calls := teeOver(io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(errBoom)))

		got, err := io.ReadAll(tee)
		if !errors.Is(err, errBoom) {
			t.Fatalf("read error = %v, want %v", err, errBoom)
		}
		if string(got) != "partial" {
			t.Fatalf("the client got %q, want %q", got, "partial")
		}
		tee.Close()

		if len(*calls) != 1 || string((*calls)[0]) != "partial" {
			t.Fatalf("OnClose calls = %q, want one with %q", *calls, "partial")
		}
	})

	t.Run("an empty body still runs OnClose, with no data", func(t *testing.T) {
		tee, calls := teeOver(strings.NewReader(""))

		io.ReadAll(tee)
		tee.Close()

		if len(*calls) != 1 || len((*calls)[0]) != 0 {
			t.Fatalf("OnClose calls = %q, want one empty call", *calls)
		}
	})

	t.Run("closes the underlying body before OnClose runs", func(t *testing.T) {
		var order []string
		tee := &UsageTeeBody{
			ReadCloser: &trackingBody{Reader: strings.NewReader("x"), onClose: func() { order = append(order, "body closed") }},
			OnClose:    func([]byte) { order = append(order, "OnClose") },
		}
		io.ReadAll(tee)

		if err := tee.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		if len(order) != 2 || order[0] != "body closed" || order[1] != "OnClose" {
			t.Fatalf("order = %v, want the body closed first, then OnClose", order)
		}
	})

	t.Run("returns the underlying Close error and still runs OnClose", func(t *testing.T) {
		errClose := errors.New("close failed")
		var got []byte
		tee := &UsageTeeBody{
			ReadCloser: &trackingBody{Reader: strings.NewReader("usage bytes"), closeErr: errClose},
			OnClose:    func(data []byte) { got = append([]byte(nil), data...) },
		}
		io.ReadAll(tee)

		if err := tee.Close(); !errors.Is(err, errClose) {
			t.Fatalf("close error = %v, want %v", err, errClose)
		}
		// A failing Close of the upstream body mustn't cost the usage record.
		if string(got) != "usage bytes" {
			t.Fatalf("OnClose got %q, want %q", got, "usage bytes")
		}
	})

	t.Run("passes bytes on as they arrive, without waiting for the end of the stream", func(t *testing.T) {
		// This is what keeps SSE real-time through the proxy: a tee that
		// buffered until EOF would hold every chunk back until the very end.
		pr, pw := io.Pipe()
		release := make(chan struct{})
		go func() {
			pw.Write([]byte("first"))
			<-release
			pw.Write([]byte("second"))
			pw.Close()
		}()
		tee := &UsageTeeBody{ReadCloser: pr, OnClose: func([]byte) {}}

		type chunk struct {
			data string
			err  error
		}
		first := make(chan chunk, 1)
		go func() {
			buf := make([]byte, 64)
			n, err := tee.Read(buf)
			first <- chunk{string(buf[:n]), err}
		}()

		select {
		case c := <-first:
			if c.err != nil || c.data != "first" {
				t.Fatalf("first read = %q, %v, want %q", c.data, c.err, "first")
			}
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("the first chunk wasn't passed on while the stream was still open")
		}

		close(release)
		rest, err := io.ReadAll(tee)
		if err != nil || string(rest) != "second" {
			t.Fatalf("rest = %q, %v, want %q", rest, err, "second")
		}
	})

	// closeEarly reads the first chunk of a stream the test writes to pw, then
	// closes the tee, as the proxy does when the client goes away, and checks
	// that Close didn't wait for the rest.
	closeEarly := func(t *testing.T) (pw *io.PipeWriter, got chan []byte) {
		t.Helper()
		pr, pw := io.Pipe()
		got = make(chan []byte, 1)
		tee := &UsageTeeBody{ReadCloser: pr, OnClose: func(data []byte) { got <- append([]byte(nil), data...) }}
		go pw.Write([]byte("content "))
		buf := make([]byte, 64)
		if n, err := tee.Read(buf); err != nil || string(buf[:n]) != "content " {
			t.Fatalf("first read = %q, %v", buf[:n], err)
		}

		closed := make(chan error, 1)
		go func() { closed <- tee.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("close: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close waited for the rest of the body")
		}
		return pw, got
	}
	waitFor := func(t *testing.T, got chan []byte) string {
		t.Helper()
		select {
		case data := <-got:
			return string(data)
		case <-time.After(2 * time.Second):
			t.Fatal("OnClose never ran")
			return ""
		}
	}

	t.Run("reads a body closed before its end on to the end, so the usage that comes last still counts", func(t *testing.T) {
		pw, got := closeEarly(t)

		pw.Write([]byte("usage"))
		pw.Close()

		if data := waitFor(t, got); data != "content usage" {
			t.Errorf("OnClose got %q, want %q", data, "content usage")
		}
	})

	t.Run("stops reading on when the body fails, and records what arrived", func(t *testing.T) {
		// What happens when the request behind the body is cancelled.
		pw, got := closeEarly(t)

		pw.CloseWithError(errors.New("context canceled"))

		if data := waitFor(t, got); data != "content " {
			t.Errorf("OnClose got %q, want %q", data, "content ")
		}
	})
}

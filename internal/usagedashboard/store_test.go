package usagedashboard

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	usage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestPersistentCostsFiltersAndDedup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	s := New(path)
	rates := []Rate{{Model: "test-model", Input: 2, CacheRead: .2, CacheWrite: 3, Output: 10, Overrides: []Override{{MinInput: 200, Input: 4, CacheRead: .4, CacheWrite: 6, Output: 15}}}}
	if err := s.SaveRates(ctx, rates); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	r := usage.Record{RequestID: "execution-1", AuthID: "credential-secret", Provider: "codex", Model: "test-model", RequestedAt: now, SessionID: "session-1", Detail: usage.Detail{TokenBreakdown: usage.NewSubsetTokenBreakdown(100, 40, 10, 20, 5, 120)}, APIKey: "must-not-store", ResponseHeaders: map[string][]string{"Secret": {"must-not-store"}}}
	// Use the canonical token breakdown to exercise non-overlapping cache and reasoning buckets.
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "execution-2"
	r.AuthID = "another-account"
	r.Provider = "claude"
	r.Failed = true
	r.Detail.TokenBreakdown = usage.NewSubsetTokenBreakdown(300, 100, 20, 30, 10, 330)
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "execution-3"
	r.Model = "unknown"
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = New(path)
	defer s.Close()
	result, err := s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Attempts != 3 || result.Totals.Tokens != 780 || result.Totals.Output != 80 || result.Totals.Reasoning != 25 || result.Totals.Unpriced != 1 || result.Sessions != 1 {
		t.Fatalf("unexpected totals: %+v", result)
	}
	// First: 50*2 + 40*.2 + 10*3 + 20*10 = 338 per million.
	// Second uses long-context rates: 180*4 + 100*.4 + 20*6 + 30*15 = 1330 per million.
	if math.Abs(result.Totals.Cost-.001668) > 1e-10 {
		t.Fatalf("cost: %.12f", result.Totals.Cost)
	}
	filtered, err := s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Hour), "codex", AccountID("codex", "credential-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Totals.Attempts != 1 || len(filtered.Models) != 1 {
		t.Fatalf("bad filter: %+v", filtered)
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), "must-not-store") || strings.Contains(string(b), "credential-secret") {
		t.Fatal("secret leaked")
	}
	rows, err := s.db.Query(`SELECT payload FROM usage_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		rows.Scan(&payload)
		if strings.Contains(payload, "must-not-store") || strings.Contains(payload, "credential-secret") {
			t.Fatal("secret persisted")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("database permissions: %v", info.Mode())
	}
}
func TestPricingChangesApplyToHistoryAndUnknownIsNotFree(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "usage.sqlite"))
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	r := usage.Record{RequestID: "one", Provider: "custom", Model: "unlisted", RequestedAt: now, Detail: usage.Detail{TokenBreakdown: usage.NewSubsetTokenBreakdown(100, 0, 0, 10, 0, 110)}}
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}
	summary, err := s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Unpriced != 1 {
		t.Fatal("unknown model treated as free")
	}
	if err = s.SaveRates(ctx, []Rate{{Model: "unlisted", Input: 10, CacheRead: 1, CacheWrite: 10, Output: 20}}); err != nil {
		t.Fatal(err)
	}
	summary, err = s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Unpriced != 0 || math.Abs(summary.Totals.Cost-.0012) > 1e-10 {
		t.Fatalf("not repriced: %+v", summary.Totals)
	}
}
func TestPriceValidationAndSeed(t *testing.T) {
	var seed []Rate
	if err := json.Unmarshal(defaultPrices, &seed); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRates(seed); err != nil {
		t.Fatal(err)
	}
	for _, rates := range [][]Rate{{{Model: "x", Input: -1}}, {{Model: "x", Input: math.Inf(1)}}, {{Model: "x"}, {Model: "x"}}, {{Model: ""}}, {{Model: "x", Overrides: []Override{{MinInput: 2}, {MinInput: 1}}}}} {
		if ValidateRates(rates) == nil {
			t.Fatalf("accepted invalid rates: %+v", rates)
		}
	}
}
func TestRetentionAndIncompleteUsage(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "usage.sqlite"))
	defer s.Close()
	now := time.Now()
	old := usage.Record{RequestID: "old", Provider: "custom", Model: "test", RequestedAt: now.AddDate(0, 0, -91)}
	if err := s.Record(old); err != nil {
		t.Fatal(err)
	}
	current := usage.Record{RequestID: "current", Provider: "custom", Model: "test", RequestedAt: now, Detail: usage.Detail{CachedTokens: 10, TotalTokens: 10}}
	if err := s.Record(current); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRates(context.Background(), []Rate{{Model: "test", Input: 1}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Summary(context.Background(), now.AddDate(0, 0, -95), now.Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Attempts != 1 || result.Totals.Unpriced != 1 {
		t.Fatalf("retention or incomplete tokens: %+v", result)
	}
}

func TestDispatcherPersistsCancelledRequestBeforeCompletion(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "usage.sqlite"))
	defer s.Close()
	mgr := usage.NewManager(8)
	mgr.Register(s)
	delivered := make(chan struct{})
	mgr.Register(completionSink(func() { close(delivered) }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC()
	mgr.Publish(ctx, usage.Record{RequestID: "async", Provider: "codex", Model: "gpt-6-astra", RequestedAt: now, Detail: usage.Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}})
	mgr.Stop()
	<-delivered
	result, err := s.Summary(context.Background(), now.Add(-time.Hour), now.Add(time.Nanosecond), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Attempts != 1 || result.Totals.Cost <= 0 {
		t.Fatalf("asynchronous event missing: %+v", result)
	}
}

type completionSink func()

func (f completionSink) HandleUsage(context.Context, usage.Record) { f() }

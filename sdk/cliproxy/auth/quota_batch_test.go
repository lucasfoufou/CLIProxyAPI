package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type batchHarness struct {
	selector *QuotaBatchSelector
	now      time.Time
	quotas   map[string]quotaBatchSnapshot
	auths    []*Auth
	failed   map[string]error
}

func batchTestHarness(t *testing.T, stateFile string) *batchHarness {
	t.Helper()
	h := &batchHarness{selector: NewQuotaBatchSelector(stateFile), now: time.Unix(1800000000, 0), quotas: map[string]quotaBatchSnapshot{}, failed: map[string]error{}}
	h.selector.now = func() time.Time { return h.now }
	h.selector.probe = func(_ context.Context, a *Auth) (quotaBatchSnapshot, error) {
		if err := h.failed[a.ID]; err != nil {
			return quotaBatchSnapshot{}, err
		}
		q, ok := h.quotas[a.ID]
		if !ok {
			return q, errors.New("unknown quota")
		}
		q.Windows = append([]quotaBatchWindow(nil), q.Windows...)
		return q, nil
	}
	return h
}
func (h *batchHarness) add(id string, short bool, used float64, resetHours int) {
	seconds := int64(604800)
	if short {
		seconds = 18000
	}
	h.quotas[id] = quotaBatchSnapshot{Windows: []quotaBatchWindow{{Used: used, Seconds: seconds, Reset: h.now.Add(time.Duration(resetHours) * time.Hour).Unix()}}}
	h.auths = append(h.auths, &Auth{ID: id, Provider: "codex", Status: StatusActive})
}
func (h *batchHarness) used(id string, used float64) {
	q := h.quotas[id]
	q.Windows[0].Used = used
	h.quotas[id] = q
}
func (h *batchHarness) pick(t *testing.T, want string) {
	t.Helper()
	h.now = h.now.Add(31 * time.Second)
	a, err := h.selector.Pick(context.Background(), "codex", "gpt-test", cliproxyexecutor.Options{}, h.auths)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != want {
		t.Fatalf("picked %s, want %s", a.ID, want)
	}
}
func TestQuotaBatchRoundsUseFifteenPointsAndEarliestReset(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("later", true, 20, 3)
	h.add("sooner", true, 30, 2)
	h.add("weekly", false, 0, 1)
	h.pick(t, "sooner")
	h.used("sooner", 44.9)
	h.pick(t, "sooner")
	h.used("sooner", 45)
	h.pick(t, "later")
	h.used("later", 35)
	h.pick(t, "sooner")
	h.used("sooner", 60)
	h.pick(t, "later")
}
func TestQuotaBatchNormalThenWeeklyThenLastReserves(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("later", true, 89, 3)
	h.add("sooner", true, 90, 2)
	h.add("weekly", false, 75, 1)
	h.pick(t, "later")
	h.used("later", 90)
	h.pick(t, "weekly")
	h.used("weekly", 90)
	h.pick(t, "sooner")
	h.used("sooner", 95)
	h.pick(t, "sooner")
	h.used("sooner", 100)
	h.pick(t, "later")
	h.used("later", 100)
	h.pick(t, "weekly")
	h.used("weekly", 100)
	h.now = h.now.Add(31 * time.Second)
	if a, err := h.selector.Pick(context.Background(), "codex", "gpt", cliproxyexecutor.Options{}, h.auths); err == nil || a != nil {
		t.Fatal("exhausted accounts must not be selected")
	}
}
func TestQuotaBatchProtectsWeeklyOnFiveHourAccounts(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("short", true, 0, 2)
	h.add("weekly", false, 20, 5)
	q := h.quotas["short"]
	q.Windows = append(q.Windows, quotaBatchWindow{Used: 90, Seconds: 604800, Reset: h.now.Add(6 * 24 * time.Hour).Unix()})
	h.quotas["short"] = q
	h.pick(t, "weekly")
	h.used("weekly", 90)
	h.pick(t, "short")
	q = h.quotas["short"]
	q.Windows[1].Used = 100
	h.quotas["short"] = q
	h.pick(t, "weekly")
}
func TestQuotaBatchRestartKeepsPartialBatchAndRound(t *testing.T) {
	file := filepath.Join(t.TempDir(), "batch-state")
	h := batchTestHarness(t, file)
	h.add("a", true, 10, 2)
	h.add("b", true, 0, 3)
	h.pick(t, "a")
	h.used("a", 25)
	h.pick(t, "b")
	h.used("b", 7)
	restored := batchTestHarness(t, file)
	restored.now = h.now
	restored.quotas = h.quotas
	restored.auths = h.auths
	restored.pick(t, "b")
	restored.used("b", 15)
	restored.pick(t, "a")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("state must be private")
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "token") {
		t.Fatal("state must not store tokens")
	}
}
func TestQuotaBatchResetRestoresNormalCapacity(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("a", true, 90, 2)
	h.add("weekly", false, 80, 3)
	h.pick(t, "weekly")
	q := h.quotas["a"]
	q.Windows[0].Used = 0
	q.Windows[0].Reset = h.now.Add(5 * time.Hour).Unix()
	h.quotas["a"] = q
	h.pick(t, "a")
}
func TestQuotaBatchUnknownQuotaDoesNotUnlockReserve(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("a", true, 90, 2)
	h.add("unknown", true, 0, 3)
	h.failed["unknown"] = errors.New("network unavailable")
	if _, err := h.selector.Pick(context.Background(), "codex", "gpt", cliproxyexecutor.Options{}, h.auths); err == nil {
		t.Fatal("unknown quota must protect reserve")
	}
	delete(h.failed, "unknown")
	h.pick(t, "unknown")
}
func TestQuotaBatchRetryExclusionsDoNotUnlockReserve(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("normal", true, 0, 2)
	h.add("reserve", false, 90, 3)
	ctx := context.WithValue(context.Background(), quotaBatchContextKey{}, quotaBatchContext{pool: h.auths})
	if _, err := h.selector.Pick(ctx, "codex", "gpt", cliproxyexecutor.Options{}, h.auths[1:]); err == nil {
		t.Fatal("retry exclusions must not unlock reserve")
	}
}
func TestQuotaBatchCorruptStateFailsClosed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	h := batchTestHarness(t, file)
	h.add("a", true, 0, 1)
	if _, err := h.selector.Pick(context.Background(), "codex", "gpt", cliproxyexecutor.Options{}, h.auths); err == nil {
		t.Fatal("corrupt state must be reported")
	}
}
func TestQuotaBatchPassiveSnapshotPreservesWeeklyGuard(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("a", true, 0, 2)
	h.add("b", false, 0, 3)
	q := h.quotas["a"]
	q.Windows = append(q.Windows, quotaBatchWindow{Used: 89, Seconds: 604800, Reset: h.now.Add(6 * 24 * time.Hour).Unix()})
	h.quotas["a"] = q
	h.pick(t, "a")
	h.auths[0].Quota = QuotaState{ObservedAt: h.now.Add(time.Second), Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "15", "X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Reset-At": fmt.Sprint(q.Windows[0].Reset),
	}}
	// Observe the partial HTTP snapshot before the next scheduled full probe.
	h.now = h.now.Add(2 * time.Second)
	if _, err := h.selector.Pick(context.Background(), "codex", "gpt", cliproxyexecutor.Options{}, h.auths); err != nil {
		t.Fatal(err)
	}
	if got := len(h.selector.snapshots["a"].Windows); got != 2 {
		t.Fatalf("lost weekly guard: %d windows", got)
	}
}
func TestQuotaBatchConcurrentPicksKeepOneBatch(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("a", true, 0, 1)
	h.add("b", true, 0, 2)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			a, err := h.selector.Pick(context.Background(), "codex", "gpt", cliproxyexecutor.Options{}, h.auths)
			if err != nil || a.ID != "a" {
				t.Errorf("concurrent selection: %v %v", a, err)
			}
		})
	}
	wg.Wait()
}
func TestQuotaBatchProbeParsesAndRejectsMalformedWindows(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"short and weekly", `{"rate_limit":{"primary_window":{"used_percent":15,"limit_window_seconds":18000,"reset_at":1800001000},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":1800600000}}}`, 200, false},
		{"weekly only", `{"rate_limit":{"primary_window":{"used_percent":37,"limit_window_seconds":604800,"reset_at":1800600000},"secondary_window":null}}`, 200, false},
		{"missing percentage", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_at":1800001000}}}`, 200, true},
		{"unknown duration", `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":42,"reset_at":1800001000}}}`, 200, true},
		{"out of range", `{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000,"reset_at":1800001000}}}`, 200, true},
		{"no windows", `{}`, 200, true}, {"denied", `{}`, 401, true}, {"rate limited", `{}`, 429, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), quotaBatchContextKey{}, quotaBatchContext{do: func(_ context.Context, _ *Auth, req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
					t.Fatal("unexpected probe target")
				}
				if req.Header.Get("ChatGPT-Account-Id") != "test-account" {
					t.Fatal("missing account scope")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}})
			_, err := probeCodexBatchQuota(ctx, &Auth{Metadata: map[string]any{"account_id": "test-account"}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("probe error=%v, want error=%v", err, tc.wantErr)
			}
		})
	}
}
func TestQuotaBatchManagerPreservesPoolForPinnedAndRetryRequests(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("normal", true, 0, 2)
	h.add("reserve", false, 90, 3)
	manager := NewManager(nil, h.selector, nil)
	manager.RegisterExecutor(&quotaAttemptIsolationExecutor{})
	model := "gpt-quota-batch-pool-test"
	for _, a := range h.auths {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "reserve"}}
	if _, err := manager.SelectAuth(context.Background(), "codex", model, opts); err == nil {
		t.Fatal("pinned reserve request bypassed full-pool policy")
	}
	if _, _, err := manager.pickNextLegacy(context.Background(), "codex", model, cliproxyexecutor.Options{}, map[string]struct{}{"normal": {}}); err == nil {
		t.Fatal("retry request bypassed full-pool policy")
	}
	if a, err := manager.SelectAuth(context.Background(), "codex", model, cliproxyexecutor.Options{}); err != nil || a.ID != "normal" {
		t.Fatalf("normal selection: %v %v", a, err)
	}
}

func TestQuotaBatchResetRoundingDoesNotRestartBatch(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("a", true, 0, 2)
	h.add("b", true, 0, 3)
	h.pick(t, "a")
	q := h.quotas["a"]
	q.Windows[0].Used = 14
	q.Windows[0].Reset += 1
	h.quotas["a"] = q
	h.pick(t, "a")
	h.used("a", 15)
	h.pick(t, "b")
}

func TestQuotaBatchFiveHourRotationIgnoresWeeklyBatchDelta(t *testing.T) {
	h := batchTestHarness(t, "")
	h.add("a", true, 10, 2)
	h.add("b", true, 0, 3)
	q := h.quotas["a"]
	q.Windows = append(q.Windows, quotaBatchWindow{Used: 20, Seconds: 604800, Reset: h.now.Add(6 * 24 * time.Hour).Unix()})
	h.quotas["a"] = q
	h.pick(t, "a")
	q = h.quotas["a"]
	q.Windows[0].Used = 14
	q.Windows[1].Used = 40
	h.quotas["a"] = q
	h.pick(t, "a") // Weekly consumption increased 20 points; the short batch used only four.
	h.used("a", 25)
	h.pick(t, "b") // Fifteen points of the five-hour window end the batch.
}

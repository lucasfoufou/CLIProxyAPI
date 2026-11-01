package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const quotaBatchPercent = 15.0
const quotaBatchReserve = 10.0
const quotaBatchProbeInterval = 30 * time.Second

type quotaBatchWindow struct {
	Used    float64 `json:"used"`
	Seconds int64   `json:"seconds"`
	Reset   int64   `json:"reset"`
}

type quotaBatchSnapshot struct {
	Windows  []quotaBatchWindow
	Observed time.Time
	Fetched  time.Time
	Rejected bool
}

func (q quotaBatchSnapshot) current(now time.Time) bool {
	if len(q.Windows) == 0 {
		return false
	}
	for _, w := range q.Windows {
		if w.Reset <= now.Unix() {
			return false
		}
	}
	return true
}

func (q quotaBatchSnapshot) control() quotaBatchWindow {
	for _, w := range q.Windows {
		if w.Seconds == 18000 {
			return w
		}
	}
	return q.Windows[0]
}

func (q quotaBatchSnapshot) remaining() float64 {
	remaining := 100.0
	for _, w := range q.Windows {
		remaining = math.Min(remaining, 100-w.Used)
	}
	return remaining
}

type quotaBatchVisit struct {
	Window string `json:"window"`
	Tier   int    `json:"tier"`
}

type quotaBatchState struct {
	Version   int                        `json:"version"`
	Current   string                     `json:"current,omitempty"`
	StartUsed float64                    `json:"start_used"`
	Window    string                     `json:"window,omitempty"`
	Tier      int                        `json:"tier"`
	Visited   map[string]quotaBatchVisit `json:"visited"`
}

// QuotaBatchSelector rotates Codex accounts in 15 percentage-point batches.
// Normal capacity is consumed before the final 10% reserve. Five-hour accounts
// precede weekly-only accounts, and each round orders accounts by earliest reset.
// One provider-wide state intentionally spans models and sessions.
// Other providers retain the normal round-robin behavior.
type QuotaBatchSelector struct {
	mu        sync.Mutex
	state     quotaBatchState
	stateFile string
	initErr   error
	snapshots map[string]quotaBatchSnapshot
	now       func() time.Time
	probe     func(context.Context, *Auth) (quotaBatchSnapshot, error)
	fallback  RoundRobinSelector
}

// NewQuotaBatchSelector restores batch progress without storing credentials.
// An empty stateFile is useful for isolated, in-memory instances and tests.
func NewQuotaBatchSelector(stateFile string) *QuotaBatchSelector {
	s := &QuotaBatchSelector{stateFile: stateFile, snapshots: make(map[string]quotaBatchSnapshot), now: time.Now}
	s.state = quotaBatchState{Version: 1, Visited: make(map[string]quotaBatchVisit)}
	if stateFile != "" {
		raw, err := os.ReadFile(stateFile)
		if err == nil {
			if len(raw) > 1<<20 {
				s.initErr = errors.New("batch state exceeds size limit")
			} else if err = json.Unmarshal(raw, &s.state); err != nil {
				s.initErr = errors.New("invalid batch state")
			}
			if s.state.Version != 1 || math.IsNaN(s.state.StartUsed) || s.state.StartUsed < 0 || s.state.StartUsed > 100 {
				s.initErr = errors.New("invalid batch state version or baseline")
			}
		} else if !os.IsNotExist(err) {
			s.initErr = errors.New("cannot read batch state")
		}
	}
	if s.state.Visited == nil {
		s.state.Visited = make(map[string]quotaBatchVisit)
	}
	s.probe = probeCodexBatchQuota
	return s
}

type quotaBatchContextKey struct{}
type quotaBatchContext struct {
	pool []*Auth
	do   func(context.Context, *Auth, *http.Request) (*http.Response, error)
}

// quotaBatchSelectionContext preserves the full eligible pool across retry
// exclusions, so a transient failure cannot prematurely unlock the reserve.
func (m *Manager) quotaBatchSelectionContext(ctx context.Context, selector Selector, pool []*Auth) context.Context {
	if _, ok := selector.(*QuotaBatchSelector); !ok {
		return ctx
	}
	return context.WithValue(ctx, quotaBatchContextKey{}, quotaBatchContext{
		pool: pool,
		do: func(ctx context.Context, a *Auth, req *http.Request) (*http.Response, error) {
			exec, ok := m.Executor(executorKeyFromAuth(a))
			if !ok {
				return nil, errors.New("quota executor unavailable")
			}
			return exec.HttpRequest(ctx, a, req)
		},
	})
}

func (s *QuotaBatchSelector) persist() error {
	if s.stateFile == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.stateFile)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".quota-batch-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.stateFile)
}

func quotaBatchWindowKey(w quotaBatchWindow) string { return fmt.Sprintf("%d:%d", w.Seconds, w.Reset) }

// Relative reset headers are rounded by the upstream service. Small timestamp
// drift must not restart a partial batch or give an account another turn.
func quotaBatchSameWindow(left, right string) bool {
	if left == right {
		return true
	}
	a, b := strings.Split(left, ":"), strings.Split(right, ":")
	if len(a) != 2 || len(b) != 2 || a[0] != b[0] {
		return false
	}
	x, errX := strconv.ParseInt(a[1], 10, 64)
	y, errY := strconv.ParseInt(b[1], 10, 64)
	return errX == nil && errY == nil && math.Abs(float64(x)-float64(y)) <= 120
}

func (s *QuotaBatchSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	available, err := getSelectorAvailableAuthsAcrossPriorities(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	var codex []*Auth
	for _, a := range available {
		if strings.EqualFold(a.Provider, "codex") {
			codex = append(codex, a)
		}
	}
	if len(codex) == 0 {
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return nil, &Error{Code: "quota_batch_state", Message: s.initErr.Error()}
	}
	pool := codex
	if info, ok := ctx.Value(quotaBatchContextKey{}).(quotaBatchContext); ok {
		pool = info.pool
	}
	known := make(map[string]quotaBatchSnapshot)
	unknown := false
	for _, a := range pool {
		if a == nil || a.Disabled || !strings.EqualFold(a.Provider, "codex") {
			continue
		}
		q := s.snapshots[a.ID]
		if passive, ok := quotaBatchPassiveSnapshot(a.Quota); ok && passive.Observed.After(q.Observed) {
			// Preserve independently observed windows when a response omits one.
			for _, old := range q.Windows {
				found := false
				for _, next := range passive.Windows {
					if next.Seconds == old.Seconds {
						found = true
						break
					}
				}
				if !found {
					passive.Windows = append(passive.Windows, old)
				}
			}
			passive.Fetched = q.Fetched
			q = passive
		}
		if q.Fetched.IsZero() || now.Sub(q.Fetched) >= quotaBatchProbeInterval || len(q.Windows) > 0 && !q.current(now) {
			fresh, errProbe := s.probe(ctx, a)
			if errProbe == nil {
				fresh.Fetched = now
				fresh.Observed = now
				q = fresh
			} else {
				// No stale fallback: loss of quota visibility must not consume a reserve.
				q = quotaBatchSnapshot{Fetched: now}
				var rejected *quotaBatchRejectedError
				if errors.As(errProbe, &rejected) {
					q.Rejected = true
					q.Fetched = now
				} else {
					unknown = true
				}
			}
		}
		s.snapshots[a.ID] = q
		if q.current(now) {
			known[a.ID] = q
		} else if !q.Rejected {
			unknown = true
		}
	}
	// Stage 0/1 uses normal five-hour/weekly capacity; stage 2/3 spends reserves.
	tier := 4
	for _, q := range known {
		if q.remaining() <= 0 {
			continue
		}
		candidateTier := 0
		if q.control().Seconds != 18000 {
			candidateTier = 1
		}
		if q.remaining() <= quotaBatchReserve {
			candidateTier += 2
		}
		if candidateTier < tier {
			tier = candidateTier
		}
	}
	if tier >= 4 || tier >= 2 && unknown {
		return nil, &Error{Code: "quota_batch_unavailable", Message: "no safe quota batch available; waiting for quota visibility or reset"}
	}
	eligible := make([]*Auth, 0, len(codex))
	for _, a := range codex {
		q, ok := known[a.ID]
		if !ok || q.remaining() <= 0 {
			continue
		}
		t := 0
		if q.control().Seconds != 18000 {
			t = 1
		}
		if q.remaining() <= quotaBatchReserve {
			t += 2
		}
		if t == tier {
			eligible = append(eligible, a)
		}
	}
	if len(eligible) == 0 {
		return nil, &Error{Code: "quota_batch_unavailable", Message: "normal capacity is temporarily unavailable; reserves remain protected"}
	}
	sort.Slice(eligible, func(i, j int) bool {
		left, right := known[eligible[i].ID].control(), known[eligible[j].ID].control()
		if left.Reset != right.Reset {
			return left.Reset < right.Reset
		}
		return eligible[i].ID < eligible[j].ID
	})
	for _, a := range eligible {
		q := known[a.ID]
		w := q.control()
		key := quotaBatchWindowKey(w)
		if a.ID == s.state.Current && tier == s.state.Tier && quotaBatchSameWindow(key, s.state.Window) && w.Used >= s.state.StartUsed &&
			(tier >= 2 || w.Used-s.state.StartUsed < quotaBatchPercent) {
			return a, nil
		}
	}
	// A finished batch counts as visited only for this reset window and stage.
	if s.state.Current != "" {
		s.state.Visited[s.state.Current] = quotaBatchVisit{Window: s.state.Window, Tier: s.state.Tier}
	}
	var picked *Auth
	for _, a := range eligible {
		v, visited := s.state.Visited[a.ID]
		if !visited || v.Tier != tier || !quotaBatchSameWindow(v.Window, quotaBatchWindowKey(known[a.ID].control())) {
			picked = a
			break
		}
	}
	if picked == nil {
		for _, a := range eligible {
			delete(s.state.Visited, a.ID)
		}
		picked = eligible[0]
	}
	w := known[picked.ID].control()
	s.state.Current = picked.ID
	s.state.StartUsed = w.Used
	s.state.Window = quotaBatchWindowKey(w)
	s.state.Tier = tier
	// Prune removed credentials to bound on-disk state.
	poolIDs := make(map[string]bool)
	for _, a := range pool {
		if a != nil {
			poolIDs[a.ID] = true
		}
	}
	for id := range s.state.Visited {
		if !poolIDs[id] {
			delete(s.state.Visited, id)
		}
	}
	if errSave := s.persist(); errSave != nil {
		s.initErr = errors.New("cannot persist quota batch state")
		return nil, &Error{Code: "quota_batch_state", Message: s.initErr.Error()}
	}
	log.WithFields(log.Fields{"auth": picked.ID, "tier": tier, "used_percent": w.Used, "reset_at": w.Reset}).Info("quota-batch: selected account")
	return picked, nil
}

type quotaBatchRejectedError struct{}

func (*quotaBatchRejectedError) Error() string { return "quota credential rejected" }

// probeCodexBatchQuota uses the registered executor's transport and auth handling.
// The fixed URL prevents credentials from being sent to a configurable third party.
func probeCodexBatchQuota(ctx context.Context, a *Auth) (quotaBatchSnapshot, error) {
	info, ok := ctx.Value(quotaBatchContextKey{}).(quotaBatchContext)
	if !ok || info.do == nil {
		return quotaBatchSnapshot{}, errors.New("quota probe transport missing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return quotaBatchSnapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	if account, ok := a.Metadata["account_id"].(string); ok && account != "" {
		req.Header.Set("ChatGPT-Account-Id", account)
	}
	resp, err := info.do(ctx, a, req)
	if err != nil {
		return quotaBatchSnapshot{}, errors.New("quota probe transport failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("quota-batch: close probe body failed")
		}
	}()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return quotaBatchSnapshot{}, &quotaBatchRejectedError{}
	}
	if resp.StatusCode != 200 {
		return quotaBatchSnapshot{}, fmt.Errorf("quota probe HTTP %d", resp.StatusCode)
	}
	var payload struct {
		RateLimit struct {
			Primary *struct {
				Used    *float64 `json:"used_percent"`
				Seconds int64    `json:"limit_window_seconds"`
				Reset   int64    `json:"reset_at"`
			} `json:"primary_window"`
			Secondary *struct {
				Used    *float64 `json:"used_percent"`
				Seconds int64    `json:"limit_window_seconds"`
				Reset   int64    `json:"reset_at"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return quotaBatchSnapshot{}, errors.New("invalid quota response")
	}
	var q quotaBatchSnapshot
	for _, w := range []*struct {
		Used    *float64 `json:"used_percent"`
		Seconds int64    `json:"limit_window_seconds"`
		Reset   int64    `json:"reset_at"`
	}{payload.RateLimit.Primary, payload.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		if w.Used == nil || math.IsNaN(*w.Used) || math.IsInf(*w.Used, 0) || *w.Used < 0 || *w.Used > 100 || (w.Seconds != 18000 && w.Seconds != 604800) || w.Reset <= 0 {
			return quotaBatchSnapshot{}, errors.New("unsupported or incomplete quota window")
		}
		q.Windows = append(q.Windows, quotaBatchWindow{Used: *w.Used, Seconds: w.Seconds, Reset: w.Reset})
	}
	if len(q.Windows) == 0 {
		return q, errors.New("no supported quota windows")
	}
	return q, nil
}

func quotaBatchPassiveSnapshot(q QuotaState) (quotaBatchSnapshot, bool) {
	headers := make(http.Header)
	for k, v := range q.Signals {
		headers.Set(k, v)
	}
	snapshot := quotaBatchSnapshot{Observed: q.ObservedAt}
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		usedText := headers.Get(prefix + "Used-Percent")
		if usedText == "" {
			continue
		}
		used, errUsed := strconv.ParseFloat(usedText, 64)
		minutes, errMinutes := strconv.ParseInt(headers.Get(prefix+"Window-Minutes"), 10, 64)
		reset, errReset := strconv.ParseInt(headers.Get(prefix+"Reset-At"), 10, 64)
		if errReset != nil {
			after, errAfter := strconv.ParseInt(headers.Get(prefix+"Reset-After-Seconds"), 10, 64)
			if errAfter != nil || after < 0 {
				return snapshot, false
			}
			reset = q.ObservedAt.Unix() + after
		}
		if errUsed != nil || errMinutes != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 || (minutes != 300 && minutes != 10080) {
			return snapshot, false
		}
		snapshot.Windows = append(snapshot.Windows, quotaBatchWindow{Used: used, Seconds: minutes * 60, Reset: reset})
	}
	return snapshot, len(snapshot.Windows) > 0 && !snapshot.Observed.IsZero()
}

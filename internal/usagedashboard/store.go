// Package usagedashboard persists sanitized usage independently of the transient management queue.
package usagedashboard

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

//go:embed prices.json
var defaultPrices []byte

// Rate contains USD prices per million tokens. Overrides apply at a prompt size threshold.
type Rate struct {
	Model      string     `json:"model"`
	Input      float64    `json:"input"`
	CacheRead  float64    `json:"cache_read"`
	CacheWrite float64    `json:"cache_write"`
	Output     float64    `json:"output"`
	Overrides  []Override `json:"overrides,omitempty"`
}
type Override struct {
	MinInput   int64   `json:"min_input"`
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

// Event deliberately excludes API keys, token hashes, headers, request bodies and client IPs.
type Event struct {
	ID        string                   `json:"id"`
	Provider  string                   `json:"provider"`
	Account   string                   `json:"account"`
	Model     string                   `json:"model"`
	Session   string                   `json:"session,omitempty"`
	At        time.Time                `json:"at"`
	Tokens    coreusage.TokenBreakdown `json:"tokens"`
	Failed    bool                     `json:"failed"`
	LatencyMS int64                    `json:"latency_ms"`
}

type Store struct {
	mu        sync.Mutex
	db        *sql.DB
	path      string
	closed    bool
	lastPrune time.Time
}

func New(path string) *Store { return &Store{path: path} }
func (s *Store) open() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("usage store closed")
	}
	if s.db != nil {
		return s.db, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	// Precreate with private permissions; SQLite's WAL inherits the database mode.
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", s.path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS usage_events(id TEXT PRIMARY KEY, at INTEGER NOT NULL, payload TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS usage_events_at ON usage_events(at);
 CREATE TABLE IF NOT EXISTS usage_rates(model TEXT PRIMARY KEY, payload TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	var defaults []Rate
	if err = json.Unmarshal(defaultPrices, &defaults); err != nil {
		db.Close()
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, r := range defaults {
		b, _ := json.Marshal(r)
		if _, err = tx.Exec(`INSERT OR IGNORE INTO usage_rates(model,payload) VALUES(?,?)`, r.Model, b); err != nil {
			tx.Rollback()
			db.Close()
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	s.db = db
	return db, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
func (s *Store) HandleUsage(_ context.Context, r coreusage.Record) {
	if err := s.Record(r); err != nil {
		log.WithError(err).Warn("usage dashboard could not persist event")
	}
}
func (s *Store) Record(r coreusage.Record) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	at := r.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	if r.RequestID == "" {
		return errors.New("usage event has no execution ID")
	}
	account := strings.TrimSpace(r.AuthID)
	if account == "" {
		account = strings.TrimSpace(r.AuthIndex)
	}
	account = AccountID(r.Provider, account)
	model := r.ResponseModel
	if model == "" {
		model = r.Model
	}
	event := Event{ID: r.RequestID, Provider: r.Provider, Account: account, Model: model, Session: r.SessionID, At: at, Tokens: coreusage.EnsureTokenBreakdownForProvider(r.Detail, r.Provider, r.ExecutorType).TokenBreakdown, Failed: r.Failed, LatencyMS: r.Latency.Milliseconds()}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO usage_events(id,at,payload) VALUES(?,?,?)`, event.ID, at.UnixNano(), payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	prune := time.Since(s.lastPrune) > time.Hour
	if prune {
		s.lastPrune = time.Now()
	}
	s.mu.Unlock()
	if prune {
		_, err = db.Exec(`DELETE FROM usage_events WHERE at < ?`, time.Now().Add(-90*24*time.Hour).UnixNano())
	}
	return err
}
func ValidateRates(rates []Rate) error {
	if len(rates) > 2000 {
		return errors.New("too many model prices")
	}
	seen := map[string]bool{}
	for _, r := range rates {
		if strings.TrimSpace(r.Model) == "" || len(r.Model) > 250 || seen[r.Model] {
			return errors.New("model names must be unique and nonempty")
		}
		seen[r.Model] = true
		values := []float64{r.Input, r.CacheRead, r.CacheWrite, r.Output}
		var threshold int64
		for _, o := range r.Overrides {
			if o.MinInput <= threshold {
				return errors.New("price thresholds must increase")
			}
			threshold = o.MinInput
			values = append(values, o.Input, o.CacheRead, o.CacheWrite, o.Output)
		}
		for _, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e6 {
				return errors.New("prices must be finite USD values between 0 and 1000000")
			}
		}
	}
	return nil
}
func (s *Store) Rates(ctx context.Context) ([]Rate, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT payload FROM usage_rates ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rates := make([]Rate, 0)
	for rows.Next() {
		var b []byte
		var r Rate
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		rates = append(rates, r)
	}
	return rates, rows.Err()
}
func (s *Store) SaveRates(ctx context.Context, rates []Rate) error {
	if err := ValidateRates(rates); err != nil {
		return err
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rates {
		b, _ := json.Marshal(r)
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_rates(model,payload) VALUES(?,?) ON CONFLICT(model) DO UPDATE SET payload=excluded.payload`, r.Model, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Metrics struct {
	Attempts       int64   `json:"attempts"`
	Failed         int64   `json:"failed"`
	Tokens         int64   `json:"tokens"`
	Input          int64   `json:"input"`
	CacheRead      int64   `json:"cache_read"`
	CacheWrite     int64   `json:"cache_write"`
	Output         int64   `json:"output"`
	Reasoning      int64   `json:"reasoning"`
	Unclassified   int64   `json:"unclassified"`
	Unpriced       int64   `json:"unpriced"`
	Cost           float64 `json:"cost"`
	InputCost      float64 `json:"input_cost"`
	CacheReadCost  float64 `json:"cache_read_cost"`
	CacheWriteCost float64 `json:"cache_write_cost"`
	OutputCost     float64 `json:"output_cost"`
	CacheSavings   float64 `json:"cache_savings"`
	LatencyMS      int64   `json:"latency_ms"`
}
type Bucket struct {
	Key string `json:"key"`
	Metrics
}
type Summary struct {
	From               time.Time `json:"from"`
	To                 time.Time `json:"to"`
	Totals             Metrics   `json:"totals"`
	Sessions           int       `json:"sessions"`
	Providers          []Bucket  `json:"providers"`
	Accounts           []Bucket  `json:"accounts"`
	Models             []Bucket  `json:"models"`
	Hours              []Bucket  `json:"hours"`
	AvailableProviders []string  `json:"available_providers"`
	AvailableAccounts  []string  `json:"available_accounts"`
	UnpricedModels     []string  `json:"unpriced_models"`
}

func (m *Metrics) add(e Event, r Rate, priced bool) {
	t := e.Tokens
	m.Attempts++
	if e.Failed {
		m.Failed++
	}
	m.Tokens += t.TotalTokens
	m.Input += t.Input.UncachedTokens
	m.CacheRead += t.Input.CacheReadTokens
	m.CacheWrite += t.Input.CacheWriteTokens
	m.Output += t.Output.TotalTokens
	m.Reasoning += t.Output.ReasoningTokens
	m.Unclassified += t.UnclassifiedTokens
	m.LatencyMS += e.LatencyMS
	if !priced || t.Quality != coreusage.TokenAccountingQualityComplete {
		m.Unpriced++
		return
	}
	for _, o := range r.Overrides {
		if t.Input.TotalTokens >= o.MinInput {
			r.Input = o.Input
			r.CacheRead = o.CacheRead
			r.CacheWrite = o.CacheWrite
			r.Output = o.Output
		}
	}
	a := float64(t.Input.UncachedTokens) * r.Input / 1e6
	b := float64(t.Input.CacheReadTokens) * r.CacheRead / 1e6
	c := float64(t.Input.CacheWriteTokens) * r.CacheWrite / 1e6
	d := float64(t.Output.TotalTokens) * r.Output / 1e6
	m.InputCost += a
	m.CacheReadCost += b
	m.CacheWriteCost += c
	m.OutputCost += d
	m.Cost += a + b + c + d
	m.CacheSavings += float64(t.Input.CacheReadTokens) * (r.Input - r.CacheRead) / 1e6
}
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func buckets(m map[string]*Metrics, byKey bool) []Bucket {
	out := make([]Bucket, 0, len(m))
	for k, v := range m {
		out = append(out, Bucket{Key: k, Metrics: *v})
	}
	sort.Slice(out, func(i, j int) bool {
		if byKey {
			return out[i].Key < out[j].Key
		}
		if out[i].Cost == out[j].Cost {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Cost > out[j].Cost
	})
	return out
}
func (s *Store) Summary(ctx context.Context, from, to time.Time, provider, account string) (Summary, error) {
	result := Summary{From: from, To: to}
	rates, err := s.Rates(ctx)
	if err != nil {
		return result, err
	}
	prices := map[string]Rate{}
	for _, r := range rates {
		prices[r.Model] = r
	}
	db, err := s.open()
	if err != nil {
		return result, err
	}
	rows, err := db.QueryContext(ctx, `SELECT payload FROM usage_events WHERE at>=? AND at<? ORDER BY at`, from.UnixNano(), to.UnixNano())
	if err != nil {
		return result, err
	}
	defer rows.Close()
	providers, accounts, models, hours := map[string]*Metrics{}, map[string]*Metrics{}, map[string]*Metrics{}, map[string]*Metrics{}
	availableProviders, availableAccounts, sessions, unpriced := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var b []byte
		var e Event
		if err = rows.Scan(&b); err != nil {
			return result, err
		}
		if err = json.Unmarshal(b, &e); err != nil {
			return result, fmt.Errorf("decode usage: %w", err)
		}
		availableProviders[e.Provider] = true
		if provider == "" || e.Provider == provider {
			availableAccounts[e.Account] = true
		}
		if provider != "" && e.Provider != provider || account != "" && e.Account != account {
			continue
		}
		r, ok := prices[e.Model]
		if !ok {
			unpriced[e.Model] = true
		}
		result.Totals.add(e, r, ok)
		if e.Session != "" {
			sessions[e.Session] = true
		}
		for _, pair := range []struct {
			m map[string]*Metrics
			k string
		}{{providers, e.Provider}, {accounts, e.Account}, {models, e.Model}, {hours, e.At.UTC().Truncate(time.Hour).Format(time.RFC3339)}} {
			if pair.m[pair.k] == nil {
				pair.m[pair.k] = &Metrics{}
			}
			pair.m[pair.k].add(e, r, ok)
		}
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	result.Sessions = len(sessions)
	result.Providers = buckets(providers, false)
	result.Accounts = buckets(accounts, false)
	result.Models = buckets(models, false)
	result.Hours = buckets(hours, true)
	result.AvailableProviders = sortedKeys(availableProviders)
	result.AvailableAccounts = sortedKeys(availableAccounts)
	result.UnpricedModels = sortedKeys(unpriced)
	return result, nil
}

// AccountID is stable across restarts without exposing credential IDs or filenames.
func AccountID(provider, id string) string {
	if id == "" {
		return "unattributed"
	}
	sum := sha256.Sum256([]byte(provider + "\x00" + id))
	return hex.EncodeToString(sum[:8])
}

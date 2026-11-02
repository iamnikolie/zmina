package codex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
)

// Tokens are App Server counters. Input includes cache reads and writes; output includes reasoning.
type Tokens struct {
	Input  int `json:"inputTokens"`
	Cached int `json:"cachedInputTokens"`
	Write  int `json:"cacheWriteInputTokens"`
	Output int `json:"outputTokens"`
	Total  int `json:"totalTokens"`
}

// Usage is an append-only observation of a thread's cumulative usage. Charge is false for restored snapshots.
// A deterministic event ID makes duplicate notifications and resumes idempotent.
type Usage struct {
	Thread  string           `json:"thread"`
	Turn    string           `json:"turn"`
	Role    string           `json:"role"`
	Group   string           `json:"group"`
	Total   Tokens           `json:"total"`
	Last    Tokens           `json:"last"`
	Charge  bool             `json:"charge"`
	Message registry.Message `json:"message"`
}

func usageID(u Usage) string {
	raw, _ := json.Marshal(struct {
		Thread, Turn string
		Total        Tokens
	}{u.Thread, u.Turn, u.Total})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func atomicJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".usage-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// SaveUsage atomically journals one snapshot. Billing deltas are derived when reading; no separate counter can drift after a crash.
func (r Registry) SaveUsage(u Usage) error {
	if u.Thread == "" {
		return fmt.Errorf("usage snapshot has no thread")
	}
	dir := filepath.Join(r.Dir, "usage", fmt.Sprintf("%x", sha256.Sum256([]byte(u.Thread))))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	path := filepath.Join(dir, usageID(u)+".json")
	if _, err = os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if u.Last.Input < 0 || u.Last.Cached < 0 || u.Last.Write < 0 || u.Last.Output < 0 || u.Total.Input < 0 || u.Total.Cached < 0 || u.Total.Write < 0 || u.Total.Output < 0 {
		return fmt.Errorf("inconsistent Codex usage counters")
	}
	return atomicJSON(path, u)
}

// UsageSessions adapts persisted Codex observations to the same cost/attribution pipeline as Claude replies.
func (r Registry) UsageSessions(since time.Time) ([]Usage, error) {
	files, err := filepath.Glob(filepath.Join(r.Dir, "usage", "*", "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Usage
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var u Usage
		if err = json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Message.At.Before(out[j].Message.At) })
	var charged []Usage
	previous := map[string]Tokens{}
	for _, u := range out {
		delta := u.Last
		if before, known := previous[u.Thread]; known {
			delta = Tokens{Input: u.Total.Input - before.Input, Cached: u.Total.Cached - before.Cached, Write: u.Total.Write - before.Write, Output: u.Total.Output - before.Output}
			if delta.Input < 0 || delta.Cached < 0 || delta.Write < 0 || delta.Output < 0 {
				delta = u.Last
			}
		}
		previous[u.Thread] = u.Total
		if !u.Charge || u.Message.At.Before(since) {
			continue
		}
		if delta.Cached+delta.Write > delta.Input {
			return nil, fmt.Errorf("inconsistent Codex cache usage for %s", u.Thread)
		}
		u.Message.PricingPrompt = u.Last.Input
		if delta.Input != u.Last.Input || delta.Output != u.Last.Output || delta.Cached != u.Last.Cached || delta.Write != u.Last.Write {
			u.Message.UnpricedReason = "usage snapshot spans multiple responses with unknown per-response prices"
		}
		u.Message.Input = delta.Input - delta.Cached - delta.Write
		u.Message.Read = delta.Cached
		u.Message.Write = delta.Write
		u.Message.Output = delta.Output
		if u.Message.Tokens() > 0 {
			charged = append(charged, u)
		}
	}
	return charged, nil
}

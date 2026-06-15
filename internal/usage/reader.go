package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Load reads usage records from the JSONL file, returning only those
// with a timestamp at or after `since`. If since is zero, all records
// are returned.
func Load(path string, since time.Time) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if !since.IsZero() && r.Timestamp.Before(since) {
			continue
		}
		records = append(records, r)
	}
	return records, scanner.Err()
}

// ModelSummary aggregates token usage for a single model.
type ModelSummary struct {
	Model        string
	Backend      string
	Requests     int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheCreate  int64
}

// Summary aggregates records by model+backend.
func Summary(records []Record) []ModelSummary {
	type key struct{ model, backend string }
	m := make(map[key]*ModelSummary)
	for _, r := range records {
		k := key{r.Model, r.Backend}
		s, ok := m[k]
		if !ok {
			s = &ModelSummary{Model: r.Model, Backend: r.Backend}
			m[k] = s
		}
		s.Requests++
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
		s.CacheRead += r.CacheRead
		s.CacheCreate += r.CacheCreate
	}
	result := make([]ModelSummary, 0, len(m))
	for _, s := range m {
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool {
		ti := result[i].InputTokens + result[i].OutputTokens
		tj := result[j].InputTokens + result[j].OutputTokens
		return ti > tj
	})
	return result
}

// ProxySummary aggregates token usage for a single proxy.
type ProxySummary struct {
	Proxy        string
	Requests     int
	InputTokens  int64
	OutputTokens int64
}

// SummaryByProxy aggregates records by proxy.
func SummaryByProxy(records []Record) []ProxySummary {
	m := make(map[string]*ProxySummary)
	for _, r := range records {
		p := r.Proxy
		if p == "" {
			p = "(unknown)"
		}
		s, ok := m[p]
		if !ok {
			s = &ProxySummary{Proxy: p}
			m[p] = s
		}
		s.Requests++
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
	}
	result := make([]ProxySummary, 0, len(m))
	for _, s := range m {
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool {
		ti := result[i].InputTokens + result[i].OutputTokens
		tj := result[j].InputTokens + result[j].OutputTokens
		return ti > tj
	})
	return result
}

// FormatTokens returns a human-friendly token count (e.g. "1.2M", "45.3k", "890").
func FormatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

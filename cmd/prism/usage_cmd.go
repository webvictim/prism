package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	usagepkg "github.com/webvictim/prism/internal/usage"
)

func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	week := fs.Bool("week", false, "show last 7 days")
	all := fs.Bool("all", false, "show all time")
	asJSON := fs.Bool("json", false, "output raw JSONL records")
	_ = fs.Parse(args)

	path, err := usagepkg.Dir()
	if err != nil {
		return err
	}

	var since time.Time
	label := "today"
	switch {
	case *all:
		label = "all time"
	case *week:
		since = time.Now().AddDate(0, 0, -7).Truncate(24 * time.Hour)
		label = "last 7 days"
	default:
		since = time.Now().Truncate(24 * time.Hour)
	}

	records, err := usagepkg.Load(path, since)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range records {
			_ = enc.Encode(r)
		}
		return nil
	}

	if len(records) == 0 {
		fmt.Printf("No usage data (%s)\n", label)
		return nil
	}

	// Totals.
	var totalIn, totalOut int64
	for _, r := range records {
		totalIn += r.InputTokens
		totalOut += r.OutputTokens
	}

	fmt.Printf("prism usage (%s, %d requests)\n", label, len(records))
	fmt.Println(repeat("─", 60))

	// By model.
	models := usagepkg.Summary(records)
	fmt.Printf("\n%-40s %8s %8s\n", "Model", "Input", "Output")
	fmt.Println(repeat("─", 60))
	for _, m := range models {
		name := m.Model
		if name == "" {
			name = "(unknown)"
		}
		fmt.Printf("%-40s %8s %8s", name, usagepkg.FormatTokens(m.InputTokens), usagepkg.FormatTokens(m.OutputTokens))
		if m.CacheRead > 0 || m.CacheCreate > 0 {
			fmt.Printf("  cache: r=%s w=%s", usagepkg.FormatTokens(m.CacheRead), usagepkg.FormatTokens(m.CacheCreate))
		}
		fmt.Println()
	}

	// By proxy.
	proxies := usagepkg.SummaryByProxy(records)
	if len(proxies) > 0 {
		fmt.Printf("\n%-40s %8s %8s\n", "Proxy", "Input", "Output")
		fmt.Println(repeat("─", 60))
		for _, p := range proxies {
			fmt.Printf("%-40s %8s %8s\n", p.Proxy, usagepkg.FormatTokens(p.InputTokens), usagepkg.FormatTokens(p.OutputTokens))
		}
	}

	fmt.Println(repeat("─", 60))
	fmt.Printf("%-40s %8s %8s\n", "TOTAL", usagepkg.FormatTokens(totalIn), usagepkg.FormatTokens(totalOut))

	return nil
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

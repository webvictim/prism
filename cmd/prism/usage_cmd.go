package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	usagepkg "github.com/webvictim/prism/internal/usage"
)

func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	week := fs.Bool("week", false, "show last 7 days")
	all := fs.Bool("all", false, "show all time")
	asJSON := fs.Bool("json", false, "output raw JSONL records")
	refresh := fs.Bool("refresh", false, "read the tenant's quota now (needs a live tsh login) rather than the daemon's copy")
	statusline := fs.Bool("statusline", false, "answer statusline's external usage request on stdin (see `prism setup statusline`)")
	_ = fs.Parse(args)

	if *statusline {
		// statusline sends its request on stdin. Run by hand there is
		// none, and reading a terminal would wait for Ctrl-D.
		var in io.Reader = os.Stdin
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, `prism: statusline sends a request on stdin; answering without one (try: echo '{"version":1}' | prism usage --statusline)`)
			in = strings.NewReader("")
		}
		return cmdUsageStatusline(in, os.Stdout)
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	if *refresh {
		// A reason it couldn't read is printed with the quota section.
		if _, err := refreshQuota(context.Background(), cfg, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "prism: could not read the tenant's quota: %v\n", err)
		}
	}

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
		printQuotaSection(os.Stdout, cfg, time.Now())
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

	// Models the gateway swapped. Its own section because the number
	// that matters is how often it happened, not the token counts: the
	// client reported (and priced) the model it asked for every time.
	if subs := usagepkg.Substitutions(records); len(subs) > 0 {
		fmt.Printf("\n%-49s %9s\n", "Substituted by gateway  [!]", "Requests")
		fmt.Println(repeat("─", 60))
		for _, sub := range subs {
			fmt.Printf("%-49s %9d\n", sub.Requested+" → "+sub.Served, sub.Requests)
		}
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

	printQuotaSection(os.Stdout, cfg, time.Now())
	return nil
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

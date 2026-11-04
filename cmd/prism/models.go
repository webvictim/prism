package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

// cmdModels implements `prism models [--cached] [--json]`: the models each
// of the cluster's LLM apps serves, from the apps' own inference
// configuration, and the model prism's probes would use with where that
// came from. Live by default, and the live answer refreshes the cache the
// daemon keeps; --cached shows that cache instead, and how old it is.
//
// It is the one place a user can see the list. The gateway has no models
// endpoint, and a model it doesn't serve is a hard 400 on a tenant with no
// fallback — which is exactly when someone needs to know what it does.
func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the lists and probe models as JSON")
	cached := fs.Bool("cached", false, "show the daemon's cached list instead of asking the cluster")
	_ = fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var apps []tshwrap.App
	var listedAs string
	var fetched time.Time
	if *cached {
		c, err := loadModelsCache()
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no cached model list yet — the daemon writes one within a couple of minutes of starting, and `prism models` writes one now")
		}
		if err != nil {
			return err
		}
		apps, listedAs, fetched = c.Apps, c.ListedAs, c.FetchedAt
	} else {
		if apps, listedAs, err = clusterAppsFrom(cfg); err != nil {
			return err
		}
		// Off means prism keeps no list; a live look is still a live look.
		if cfg.ModelDiscoveryEnabled() {
			if err := saveModelsCache(apps, listedAs, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "prism: could not update the cached model list: %v\n", err)
			}
		}
	}
	// Probe defaults from the list just read, rather than reading it again
	// — or from no list, as the probes themselves would when discovery is
	// off, so this shows what they'd really send.
	served := servedModels(apps)
	if !cfg.ModelDiscoveryEnabled() {
		served = nil
	}
	anthropic, openai := probeDefaultsFrom(cfg, func() (map[string][]string, error) { return served, nil })

	report := modelsReport{ListedAs: listedAs, Apps: llmApps(apps), Discovery: cfg.ModelDiscoveryEnabled()}
	if !fetched.IsZero() {
		report.CachedAt = &fetched
	}
	report.Probe.Anthropic = probeJSON(anthropic)
	report.Probe.OpenAI = probeJSON(openai)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	writeModels(os.Stdout, report)
	return nil
}

type modelsReport struct {
	ListedAs  string     `json:"listed_as"`
	CachedAt  *time.Time `json:"cached_at,omitempty"` // set for --cached
	Discovery bool       `json:"discovery"`
	Apps      []modelApp `json:"apps"`
	Probe     struct {
		Anthropic probeEntry `json:"anthropic"`
		OpenAI    probeEntry `json:"openai"`
	} `json:"probe"`
}

type modelApp struct {
	Name     string                   `json:"name"`
	Format   string                   `json:"format"`
	Provider string                   `json:"provider,omitempty"`
	Models   []tshwrap.InferenceModel `json:"models"`
}

type probeEntry struct {
	Model  string `json:"model"`
	Source string `json:"source"`
}

func probeJSON(c probeChoice) probeEntry { return probeEntry{c.Model, string(c.Source)} }

// llmApps keeps the apps that carry an inference configuration, prism's
// two first and the rest by name.
func llmApps(apps []tshwrap.App) []modelApp {
	var out []modelApp
	for _, a := range apps {
		if a.Spec.Inference == nil {
			continue
		}
		out = append(out, modelApp{
			Name:     a.Metadata.Name,
			Format:   a.Spec.Inference.Format,
			Provider: a.Spec.Inference.Provider,
			Models:   a.Spec.Inference.Models,
		})
	}
	rank := func(name string) int {
		switch name {
		case router.AnthropicAppName:
			return 0
		case router.OpenAIAppName:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i].Name), rank(out[j].Name); ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func writeModels(w io.Writer, r modelsReport) {
	if r.CachedAt != nil {
		age := time.Since(*r.CachedAt).Round(time.Minute)
		when := "just now"
		if age >= time.Minute {
			when = strings.TrimSuffix(age.String(), "0s") + " ago"
		}
		fmt.Fprintf(w, "Cached %s (%s), as %s.\n", when, r.CachedAt.Local().Format("2006-01-02 15:04"), r.ListedAs)
		if !r.Discovery {
			fmt.Fprintln(w, "Model discovery is off (model_discovery=false), so nothing uses or refreshes this copy.")
		} else if age > 2*modelsRefreshEvery {
			fmt.Fprintln(w, "That's older than the daemon should let it get; `prism logs` says why it isn't refreshing.")
		}
		fmt.Fprintln(w)
	}
	if len(r.Apps) == 0 {
		fmt.Fprintln(w, "No LLM apps visible. A tsh older than 18.8.3 lists the apps without their models.")
	}
	for i, app := range r.Apps {
		if i > 0 {
			fmt.Fprintln(w)
		}
		head := app.Name + "  (" + app.Format
		if app.Provider != "" {
			head += ", via " + app.Provider
		}
		fmt.Fprintln(w, head+")")
		rows := modelRows(app.Models)
		if len(rows) == 0 {
			fmt.Fprintln(w, "  no model list: the gateway passes any model name through")
			continue
		}
		width := 0
		for _, row := range rows {
			width = max(width, len(row.name))
		}
		for _, row := range rows {
			fmt.Fprintf(w, "  %-*s  %s\n", width, row.name, row.detail)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Probe model for prism test, doctor and tbot reset:")
	fmt.Fprintf(w, "  anthropic  %-16s %s\n", r.Probe.Anthropic.Model, sourceNote(r.Probe.Anthropic.Source, "anthropic_test_model", r.Discovery))
	fmt.Fprintf(w, "  openai     %-16s %s\n", r.Probe.OpenAI.Model, sourceNote(r.Probe.OpenAI.Source, "openai_test_model", r.Discovery))
	if r.CachedAt == nil {
		fmt.Fprintf(w, "\nListed as %s.\n", r.ListedAs)
	}
}

func sourceNote(source, key string, discovery bool) string {
	switch probeSource(source) {
	case sourceConfig:
		return "(set by " + key + ")"
	case sourceCluster:
		return "(from the cluster's list)"
	}
	if !discovery {
		return "(prism's built-in: model discovery is off, so the list isn't used)"
	}
	return "(prism's built-in: the cluster lists no models for this app)"
}

type modelRow struct{ name, detail string }

// modelRows lists a model's request name with the provider model it maps
// to, folding in the bare aliases that only spell that provider name out
// again — Beams lists both `claude-sonnet-5` and
// `anthropic.claude-sonnet-5` for the one model, which reads as
// `claude-sonnet-5  or anthropic.claude-sonnet-5`.
func modelRows(models []tshwrap.InferenceModel) []modelRow {
	providers := map[string]bool{}
	for _, m := range models {
		if m.ProviderName != "" {
			providers[strings.ToLower(m.ProviderName)] = true
		}
	}
	var rows []modelRow
	for _, m := range models {
		switch {
		case m.Name == "":
		case m.ProviderName != "":
			// Either name works when the provider name is listed too;
			// otherwise it is only where requests end up.
			detail := "→ " + m.ProviderName
			if hasAlias(models, m.ProviderName) {
				detail = "or " + m.ProviderName
			}
			rows = append(rows, modelRow{m.Name, detail})
		case !providers[strings.ToLower(m.Name)]:
			rows = append(rows, modelRow{m.Name, ""})
		}
	}
	return rows
}

func hasAlias(models []tshwrap.InferenceModel, provider string) bool {
	for _, m := range models {
		if m.ProviderName == "" && strings.EqualFold(m.Name, provider) {
			return true
		}
	}
	return false
}

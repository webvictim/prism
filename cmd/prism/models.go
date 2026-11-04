package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

// cmdModels implements `prism models [--json]`: the models each of the
// cluster's LLM apps serves, from the apps' own inference configuration,
// and the model prism's probes would use with where that came from.
//
// It is the one place a user can see the list. The gateway has no models
// endpoint, and a model it doesn't serve is a hard 400 on a tenant with no
// fallback — which is exactly when someone needs to know what it does.
func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the lists and probe models as JSON")
	_ = fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	apps, listedAs, err := clusterAppsFrom(cfg)
	if err != nil {
		return err
	}
	// Probe defaults from the list just read, rather than reading it again.
	served := servedModels(apps)
	anthropic, openai := probeDefaultsFrom(cfg, func() (map[string][]string, error) { return served, nil })

	report := modelsReport{ListedAs: listedAs, Apps: llmApps(apps)}
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
	ListedAs string     `json:"listed_as"`
	Apps     []modelApp `json:"apps"`
	Probe    struct {
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
	fmt.Fprintf(w, "  anthropic  %-16s %s\n", r.Probe.Anthropic.Model, sourceNote(r.Probe.Anthropic.Source, "anthropic_test_model"))
	fmt.Fprintf(w, "  openai     %-16s %s\n", r.Probe.OpenAI.Model, sourceNote(r.Probe.OpenAI.Source, "openai_test_model"))
	fmt.Fprintf(w, "\nListed as %s.\n", r.ListedAs)
}

func sourceNote(source, key string) string {
	switch probeSource(source) {
	case sourceConfig:
		return "(set by " + key + ")"
	case sourceCluster:
		return "(from the cluster's list)"
	}
	return "(prism's built-in; the cluster's list was empty)"
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

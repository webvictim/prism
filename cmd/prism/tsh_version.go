package main

import (
	"cmp"

	"github.com/webvictim/prism/internal/tshwrap"
)

// minTshForModels is the first tsh that shows an LLM app's model list
// (`spec.inference`, gravitational/teleport#66909). An older one lists the
// apps without it, and every reader of the list — `prism models`, Codex
// discovery, the probe defaults, `prism setup` — finds no models.
const minTshForModels = "18.8.3"

// The installed tsh isn't necessarily the one that runs. With Teleport's
// managed client-tools updates on, it re-executes into the version
// recorded for the cluster at the last `tsh login`, older or not, and
// keeps doing so after the cluster moves on — seen on a machine with
// 18.11.9 installed running 18.8.1 for every command, while the cluster
// was asking for 18.11.7. Nothing tsh prints says so unless asked, so
// prism asks (tshwrap.ClientVersions) and names the way out.

// tshVerdict judges the tsh that runs prism's commands. clusterTools is
// the cluster's current managed-updates version, "" when unknown. It
// returns whether that tsh is too old to list models, a description,
// and the fix when it is.
func tshVerdict(v tshwrap.ClientVersion, clusterTools, proxy string) (old bool, detail, fix string) {
	detail = "v" + v.Ran
	if v.Reexecuted() {
		detail += ", run in place of the installed v" + v.Installed + " by Teleport's managed client-tools updates"
	}
	if !versionLess(v.Ran, minTshForModels) {
		return false, detail, ""
	}
	detail += " — a tsh before v" + minTshForModels + " lists the LLM apps without their models, so prism finds none"
	login := "`tsh login --proxy=" + withDefaultPort(cmp.Or(proxy, "<proxy>")) + "`"
	switch {
	case !v.Reexecuted():
		fix = "upgrade Teleport's client tools to v" + minTshForModels + " or newer"
	case clusterTools != "" && !versionLess(clusterTools, minTshForModels):
		fix = "run " + login + " again: it records the cluster's current v" + clusterTools +
			" (or set TELEPORT_TOOLS_VERSION=off, in prism's service environment too, to use the installed tsh)"
	case clusterTools != "":
		fix = "the cluster pins tsh to v" + clusterTools + "; set TELEPORT_TOOLS_VERSION=off where prism runs, its service included, to use the installed tsh"
	default:
		fix = "run " + login + " again to record the cluster's current version, or set TELEPORT_TOOLS_VERSION=off where prism runs to use the installed tsh"
	}
	return true, detail, fix
}

// checkTshVersion is doctor's line for it. A warning at worst: every
// reader of the model list fails open, so nothing stops working, but
// model discovery is quietly off.
func checkTshVersion(r *report, proxy string) {
	v, err := tshwrap.ClientVersions()
	if err != nil {
		r.warn("tsh version", "could not run `tsh version`: "+err.Error(),
			"install Teleport's client tools; prism reads the cluster's model list with tsh")
		return
	}
	clusterTools := clusterToolsVersion(proxy)
	if old, detail, fix := tshVerdict(v, clusterTools, proxy); old {
		r.warn("tsh version", detail, fix)
	} else {
		r.ok("tsh version", detail)
	}
}

// clusterToolsVersion is the tsh version the cluster's managed updates
// currently ask for, or "" when they're off or the cluster can't be asked.
func clusterToolsVersion(proxy string) string {
	if proxy == "" {
		return ""
	}
	f, err := clusterFind(proxy)
	if err != nil || !f.AutoUpdate.ToolsAutoUpdate {
		return ""
	}
	return f.AutoUpdate.ToolsVersion
}

// tshModelsHint explains an app listing that came back without model
// lists: the tsh's verdict when it is the cause, else "".
func tshModelsHint(proxy string) string {
	v, err := tshwrap.ClientVersions()
	if err != nil {
		return ""
	}
	clusterTools := clusterToolsVersion(proxy)
	old, detail, fix := tshVerdict(v, clusterTools, proxy)
	if !old {
		return ""
	}
	return "tsh is " + detail + ".\nTo fix: " + fix + "."
}

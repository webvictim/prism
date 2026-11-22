package beam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/egress"
)

// ControlHeader must be present on the mutating control requests. The
// router listens on loopback, but a web page can still send it simple
// cross-origin POSTs; requiring a custom header forces a CORS preflight
// that prism never answers, so only local programs can start or end a
// beam. Beams are billed to their owner.
const ControlHeader = "X-Prism-Control"

// ControlPath is where the router mounts Handler.
const ControlPath = "/_prism/beam"

// Handler serves the daemon's beam controls:
//
//	GET  /_prism/beam        status
//	POST /_prism/beam/start  warm a beam for the next session (off mode:
//	                         start the one beam)
//	POST /_prism/beam/end    end the caller's repository's beams, or all
//
// The CLI names its repository in egress.WorkspaceHeader.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, ControlPath), "/")
		if action == "" {
			if r.Method != http.MethodGet {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
				return
			}
			writeJSON(w, http.StatusOK, m.Status(r.Header.Get(egress.WorkspaceHeader)))
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		if r.Header.Get(ControlHeader) == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing " + ControlHeader + " header"})
			return
		}
		// Creating a beam and opening its forwards takes a while, and the
		// client may give up; the work should finish regardless.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
		defer cancel()
		ws := r.Header.Get(egress.WorkspaceHeader)
		switch action {
		case "start":
			if _, err := m.Start(ctx, ws); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, m.Status(ws))
		case "end":
			recs, err := m.End(ctx, ws)
			if errors.Is(err, ErrNoBeam) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ended": recs, "status": m.Status(ws)})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action " + action})
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

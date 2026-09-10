package router

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/capture"
	"github.com/webvictim/prism/internal/chatcompat"
	"github.com/webvictim/prism/internal/usage"
)

// observeRequests logs every proxied request on one line, and for model
// calls records token usage as part of the same line.
//
// Logging and usage capture are deliberately one middleware rather than
// two. The token counts are only known once the response has streamed
// through, so as separate layers the log line and the usage line had to
// be printed by different wrappers — two writes per request that
// interleave under concurrency, leaving no reliable way to pair them.
func observeRequests(next http.Handler, logger *log.Logger, uw *usage.Writer, proxy string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Log anything we proxy. Gating on a /v1/ allowlist used to hide
		// whole clients: a request prism forwards but does not recognise
		// left no trace at all. Only prism's own endpoints are skipped.
		if isInternalPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		reqSize := r.Header.Get("Content-Length")
		if reqSize == "" {
			reqSize = "?"
		}

		sr := capture.NewStatusRecorder(w)

		// Usage capture applies to model calls only. Paths are
		// canonicalised upstream of here, so /v1 is the only spelling
		// that reaches this test.
		var rw http.ResponseWriter = sr
		var cw *capture.Writer
		if uw != nil && r.Method == http.MethodPost && isAPIPath(r.URL.Path) {
			backend := capture.BackendAnthropic
			if isOpenAIPath(r.URL.Path) {
				backend = capture.BackendOpenAI
			}
			cw = capture.New(sr, capture.Options{
				Backend:     backend,
				Model:       capture.ExtractModel(r),
				Proxy:       proxy,
				UsageWriter: uw,
			})
			rw = cw
		}

		// Handlers that forward to a different upstream path (the
		// chat/completions → Responses shim) record it here so the log
		// shows the translation.
		note := &chatcompat.PathNote{}
		next.ServeHTTP(rw, r.WithContext(chatcompat.WithPathNote(r.Context(), note)))

		var via string
		if note.Upstream != "" && note.Upstream != r.URL.Path {
			via = fmt.Sprintf(" [-> %s]", note.Upstream)
		}

		// The usage fields are present for every model call, zeros
		// included, so the line can be parsed without first checking
		// which fields it happens to carry.
		var fields string
		if cw != nil {
			rec, _ := cw.Finalize()
			fields = " " + capture.Summary(rec)
		}

		logger.Printf("%s %s%s %d req=%sB resp=%dB%s %s",
			r.Method, r.URL.Path, via, sr.Status(), reqSize, sr.Bytes(), fields,
			time.Since(start).Round(time.Millisecond))
	})
}

// isAPIPath reports whether a path is a versioned API endpoint. Paths
// are canonicalised upstream of this middleware, so the /v1 prefix is
// the only spelling that reaches it.
func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/")
}

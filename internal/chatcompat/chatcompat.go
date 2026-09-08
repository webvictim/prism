// Package chatcompat serves /v1/chat/completions against gateways that
// expose OpenAI models only on the Responses API (/v1/responses).
//
// It exists because the cluster's OpenAI gateway rejects every model on
// /v1/chat/completions ("model ... isn't supported on this route") while
// serving all of them on /v1/responses. Clients that only speak
// chat/completions — MacWhisper, Teleport session summaries — would
// otherwise be dead through prism.
//
// The package deliberately knows no model names. Where a model rejects a
// parameter, that's discovered from the gateway's own error message and
// remembered for the process lifetime.
package chatcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// maxParamRetries bounds the adaptive drop-and-retry loop.
const maxParamRetries = 4

// unsupportedParamRe matches the gateway's way of naming a parameter a
// model won't accept, e.g.
//
//	Unsupported parameter: 'temperature' is not supported with this model.
var unsupportedParamRe = regexp.MustCompile(`Unsupported parameter: '([^']+)'`)

// Handler serves /v1/chat/completions by talking to the OpenAI gateway
// through prism's local tunnel listener.
type Handler struct {
	// Upstream is the tunnel base URL (http://127.0.0.1:<openai port>).
	Upstream *url.URL
	Client   *http.Client
	Logger   *log.Logger
	Debug    bool
	// Translate turns chat/completions into Responses API calls. When
	// false the request is relayed to /v1/chat/completions unchanged,
	// for gateways that still serve that route natively.
	Translate bool

	mu sync.Mutex
	// rejected remembers, per model, which parameters the gateway has
	// refused, so the retry cost is paid once rather than per request.
	rejected map[string]map[string]bool
}

// New builds a Handler pointing at the OpenAI tunnel on port.
func New(port int, logger *log.Logger, debug, translate bool) *Handler {
	upstream, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Handler{
		Upstream:  upstream,
		Client:    &http.Client{Timeout: 10 * time.Minute},
		Logger:    logger,
		Debug:     debug,
		Translate: translate,
		rejected:  make(map[string]map[string]bool),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "prism: /v1/chat/completions requires POST")
		return
	}

	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "prism: read request body: "+err.Error())
		return
	}

	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "prism: parse request body: "+err.Error())
		return
	}

	if hasTools(req) {
		writeError(w, http.StatusBadRequest,
			"prism: tool calling isn't supported through the chat/completions shim — "+
				"send tool-using requests to /v1/responses instead")
		return
	}

	model, _ := req["model"].(string)
	streaming, _ := req["stream"].(bool)

	path := "/v1/chat/completions"
	if h.Translate {
		path = "/v1/responses"
		req, err = translateRequest(req, h.Logger, h.Debug)
		if err != nil {
			writeError(w, http.StatusBadRequest, "prism: "+err.Error())
			return
		}
	}
	h.dropKnownRejected(model, req)
	notePath(r.Context(), path)

	for attempt := 0; ; attempt++ {
		resp, err := h.post(r.Context(), path, req, streaming)
		if err != nil {
			h.Logger.Printf("chatcompat: upstream error: %v", err)
			writeError(w, http.StatusBadGateway, "prism: openai gateway unavailable: "+err.Error())
			return
		}

		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			// The gateway names parameters the model won't take. Drop
			// the named one and try again — this is what keeps
			// per-model quirks out of prism's source.
			if field, ok := h.unsupported(body); ok && attempt < maxParamRetries {
				if _, present := req[field]; present {
					delete(req, field)
					h.remember(model, field)
					h.Logger.Printf("chatcompat: %s rejected parameter %q — dropped it, retrying, "+
						"and remembering for the rest of this daemon's life", displayModel(model), field)
					continue
				}
			}
			// Log it: the status reaching the client is the gateway's,
			// so without this a transient upstream 500 is
			// indistinguishable from one of prism's own.
			h.Logger.Printf("chatcompat: upstream %s %d for %s: %s",
				path, resp.StatusCode, displayModel(model), truncate(body, 300))
			relayError(w, resp, body)
			return
		}

		defer resp.Body.Close()
		switch {
		case !h.Translate:
			relayVerbatim(w, resp)
		case streaming:
			h.translateStream(w, resp.Body)
		default:
			h.translateBody(w, resp)
		}
		return
	}
}

// post sends body to the upstream path.
func (h *Handler) post(ctx context.Context, path string, body map[string]any, streaming bool) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Upstream.String()+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	// A fresh request with only the headers the gateway needs: client
	// Authorization / X-Api-Key must never be forwarded, since the
	// tunnel authenticates with mTLS and a dummy token gets rejected.
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return h.Client.Do(req)
}

// translateBody converts a non-streaming Responses reply and writes it.
func (h *Handler) translateBody(w http.ResponseWriter, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "prism: read openai gateway reply: "+err.Error())
		return
	}
	out, err := translateResponse(body)
	if err != nil {
		h.Logger.Printf("chatcompat: %v", err)
		writeError(w, http.StatusBadGateway, "prism: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// --- rejected-parameter memory ---

func (h *Handler) unsupported(body []byte) (string, bool) {
	m := unsupportedParamRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

func (h *Handler) remember(model, field string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rejected[model] == nil {
		h.rejected[model] = make(map[string]bool)
	}
	h.rejected[model][field] = true
}

// dropKnownRejected removes parameters this model has already refused.
func (h *Handler) dropKnownRejected(model string, req map[string]any) {
	h.mu.Lock()
	fields := h.rejected[model]
	known := make([]string, 0, len(fields))
	for f := range fields {
		known = append(known, f)
	}
	h.mu.Unlock()

	// Logged at debug only: the learning event itself is logged
	// unconditionally, and repeating it on every subsequent request
	// would drown the log.
	for _, f := range known {
		if _, ok := req[f]; ok {
			delete(req, f)
			if h.Debug {
				h.Logger.Printf("chatcompat: pre-emptively dropped %q for %s (learned earlier)", f, displayModel(model))
			}
		}
	}
}

// --- helpers ---

// hasTools reports whether the request asks for tool calling.
func hasTools(req map[string]any) bool {
	for _, k := range []string{"tools", "functions"} {
		if v, ok := req[k]; ok {
			if list, isList := v.([]any); isList && len(list) == 0 {
				continue
			}
			if v != nil {
				return true
			}
		}
	}
	return false
}

// truncate keeps log lines bounded when a gateway returns a long body.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func displayModel(model string) string {
	if model == "" {
		return "(gateway default)"
	}
	return model
}

// relayVerbatim copies an upstream reply through unchanged, flushing as
// it goes so streamed replies aren't buffered.
func relayVerbatim(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// relayError passes the gateway's own status and message through, so its
// diagnostics reach the user rather than being replaced by prism's.
func relayError(w http.ResponseWriter, resp *http.Response, body []byte) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": msg,
		},
	})
}

// --- request-log annotation ---

// PathNote carries the upstream path a request was forwarded to, so the
// router's request log can show the translation. The router attaches an
// empty one to the request context; this handler fills it in.
type PathNote struct {
	Upstream string
}

type pathNoteKey struct{}

// WithPathNote returns a context carrying n for the handler to populate.
func WithPathNote(ctx context.Context, n *PathNote) context.Context {
	return context.WithValue(ctx, pathNoteKey{}, n)
}

// notePath records the upstream path on the context's PathNote, if any.
func notePath(ctx context.Context, path string) {
	if n, ok := ctx.Value(pathNoteKey{}).(*PathNote); ok && n != nil {
		n.Upstream = path
	}
}

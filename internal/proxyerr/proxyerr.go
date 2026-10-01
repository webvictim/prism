// Package proxyerr classifies httputil.ReverseProxy errors that are the
// client's doing rather than the gateway's.
//
// Both the local router and the MITM forward proxy front the same
// tunnels, so a client that aborts its own request has to be reported
// identically whichever way it reached prism — the same reason
// internal/scrub and internal/capture are shared. Four ErrorHandlers
// (one in the router, three in the forward proxy) would otherwise each
// carry their own copy of this check.
package proxyerr

import (
	"context"
	"errors"
	"log"
	"net/http"
)

// StatusClientClosedRequest is the non-standard code nginx uses for "the
// client went away before we answered". net/http has no constant for it.
const StatusClientClosedRequest = 499

// IsClientCanceled reports whether err is a client that aborted its own
// request. httputil.ReverseProxy derives the outbound request's context
// from the inbound one, so an aborted inbound request cancels the
// outbound call and arrives at ErrorHandler as context.Canceled.
//
// context.DeadlineExceeded is deliberately excluded: a timeout talking
// to the gateway is a real upstream failure and must still 502.
func IsClientCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// HandleClientCanceled logs and answers a client cancel, reporting
// whether it did. An ErrorHandler calls it first and returns early when
// it reports true, leaving the 502 path for genuine upstream failures:
//
//	if proxyerr.HandleClientCanceled(w, r, err, logger, "router: anthropic") {
//		return
//	}
//
// No body is written — the client that would read it has already gone.
func HandleClientCanceled(w http.ResponseWriter, r *http.Request, err error, logger *log.Logger, label string) bool {
	if !IsClientCanceled(err) {
		return false
	}
	if logger != nil {
		logger.Printf("%s: client canceled %s %s", label, r.Method, r.URL.Path)
	}
	w.WriteHeader(StatusClientClosedRequest)
	return true
}

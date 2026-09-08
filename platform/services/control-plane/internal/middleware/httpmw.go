// Package middleware: CORS + request logging + panic recovery.
package middleware

import (
	"log"
	"net/http"
	"strings"
	"time"

	"universalengagement/control-plane/internal/db"
)

// CORS adds permissive-but-configurable CORS headers. Any browser-served
// admin UI requires this (the famous missing-CORS lesson).
func CORS(allowedOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if allowedOrigin != "" {
					w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Request-Id")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Logging wraps handlers with structured request logging + request IDs.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = db.NewID("req")
		}
		w.Header().Set("X-Request-Id", reqID)

		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)

		log.Printf("%s %s -> %d (%s) [%s] actor=%s",
			r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), reqID,
			actorLabel(r))
	})
}

// Recover converts panics into 500s (never kill the process).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, err)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"an unexpected internal error occurred — the request id in the response headers can be used for support")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func actorLabel(r *http.Request) string {
	a := ActorFrom(r.Context())
	if a == nil {
		return "anonymous"
	}
	return a.KeyID
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// TrimScope is a helper for scope parsing ("project.read,events.ingest").
func TrimScope(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

package admin

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/whereiskurt/meshtk/internal/credcache"
)

// Server provides admin HTTP endpoints for cache management.
type Server struct {
	cache *credcache.Cache
	store credcache.CredentialStore
	auth  *credcache.CacheAuthenticator
	log   *log.Logger
}

// NewServer creates a new admin Server. If logger is nil, a default logger is used.
func NewServer(cache *credcache.Cache, store credcache.CredentialStore, auth *credcache.CacheAuthenticator, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		cache: cache,
		store: store,
		auth:  auth,
		log:   logger,
	}
}

// Handler returns an http.Handler with all admin routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /cache/credentials/{username}", s.handleEvict)
	mux.HandleFunc("POST /cache/credentials/{username}/refresh", s.handleRefresh)
	mux.HandleFunc("GET /cache/stats", s.handleStats)
	return s.withLogging(mux)
}

// handleEvict removes a credential from the cache.
// Always returns 200 (idempotent).
func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")

	_, existed := s.cache.Get(username)
	s.cache.Delete(username)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"username": username,
		"evicted":  existed,
	})
}

// handleRefresh re-fetches a credential from DynamoDB and updates the cache.
// Bypasses cache and circuit breaker (direct store access).
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")

	cred, err := s.store.Fetch(r.Context(), username)
	if err != nil {
		if errors.Is(err, credcache.ErrNotFound) {
			s.cache.Delete(username)
			writeError(w, http.StatusNotFound, "credential not found in store")
			return
		}
		writeError(w, http.StatusBadGateway, "store fetch failed")
		return
	}

	s.cache.Set(username, cred)
	s.auth.ResetCircuitBreaker()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"username":  username,
		"refreshed": true,
	})
}

// handleStats returns cache performance statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.cache.Stats()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries":   s.cache.Size(),
		"hits":      st.Hits,
		"misses":    st.Misses,
		"hit_rate":  st.HitRate,
		"evictions": st.Evictions,
	})
}

// writeJSON writes a success JSON response with the envelope format.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"data":      data,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// writeError writes an error JSON response with the envelope format.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":     msg,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// statusWriter captures the HTTP status code for logging.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.code = code
	sw.ResponseWriter.WriteHeader(code)
}

// withLogging wraps a handler with request logging middleware.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Printf("[INFO] admin %s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start))
	})
}

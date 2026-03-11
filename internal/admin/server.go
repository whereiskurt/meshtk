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
	mux.HandleFunc("GET /cache/credentials", s.handleListCredentials)
	mux.HandleFunc("DELETE /cache/credentials", s.handleFlushCredentials)
	mux.HandleFunc("DELETE /cache/credentials/{username}", s.handleEvict)
	mux.HandleFunc("POST /cache/credentials/{username}/refresh", s.handleRefresh)
	mux.HandleFunc("GET /cache/stats", s.handleStats)
	mux.HandleFunc("GET /health", s.handleHealth)
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

// handleListCredentials returns all cached entries with username, TTL, and negative flag.
// Passwords are never included in the response.
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	entries := s.cache.Entries()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count":   len(entries),
		"entries": entries,
	})
}

// handleFlushCredentials removes all entries from the cache.
// Stats counters are preserved (cumulative lifetime counters).
func (s *Server) handleFlushCredentials(w http.ResponseWriter, r *http.Request) {
	count := s.cache.DeleteAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"evicted_count": count,
		"stats_reset":   false,
	})
}

// handleHealth returns service health status. Always returns HTTP 200 so ECS
// health checks won't kill the task during DynamoDB outages.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "healthy"
	dynamo := "reachable"
	if s.auth.IsDegraded() {
		status = "degraded"
		dynamo = "unreachable"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        status,
		"dynamodb":      dynamo,
		"cache_entries": s.cache.Size(),
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

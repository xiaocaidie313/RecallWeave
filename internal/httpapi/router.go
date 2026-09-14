package httpapi

import "net/http"

// NewRouter connects URLs to their handlers. Keeping routing in one place makes
// it easy to see which capabilities the backend currently exposes.
func NewRouter() http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /healthz", healthHandler)

	return router
}

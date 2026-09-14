package httpapi

import (
	"encoding/json"
	"net/http"
)

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

func healthHandler(responseWriter http.ResponseWriter, _ *http.Request) {
	responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")

	if err := json.NewEncoder(responseWriter).Encode(healthResponse{
		Status:  "ok",
		Service: "recallweave-api",
	}); err != nil {
		http.Error(responseWriter, "failed to encode response", http.StatusInternalServerError)
	}
}

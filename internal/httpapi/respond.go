package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorBody is the single error envelope used by every endpoint.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Details   any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details any) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code:      code,
		Message:   msg,
		RequestID: requestID(r),
		Details:   details,
	}})
}

// requestID returns the id assigned by the request-id middleware.
// Until KAN-29 lands it falls back to the inbound header, if any.
func requestID(r *http.Request) string {
	return r.Header.Get("X-Request-Id")
}

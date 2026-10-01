package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const defaultBodyLimit = 16 << 10

// decodeJSON reads one JSON object into dst. It writes the error response
// itself and returns false when the body is unusable. Unknown fields are
// ignored on purpose: a stray user_id must not fail the request, it is
// detected and logged as a spoof attempt by the handler instead.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large", nil)
		case errors.Is(err, io.EOF):
			writeError(w, r, http.StatusBadRequest, "invalid_json", "request body is empty", nil)
		default:
			writeError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint", nil)
		}
		return false
	}
	if dec.More() {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must be a single JSON object", nil)
		return false
	}
	return true
}

// fieldErrors collects validation problems keyed by field name and is
// returned as 422 details.
type fieldErrors map[string]string

func (f fieldErrors) add(field, msg string) {
	if _, exists := f[field]; !exists {
		f[field] = msg
	}
}

func (f fieldErrors) write(w http.ResponseWriter, r *http.Request) bool {
	if len(f) == 0 {
		return false
	}
	writeError(w, r, http.StatusUnprocessableEntity, "validation_failed", "request has invalid fields", f)
	return true
}

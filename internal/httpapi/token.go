package httpapi

import (
	"net/http"
	"time"

	"github.com/tanmayt124/seat-reservation/internal/auth"
)

const testTokenTTL = time.Hour

type tokenRequest struct {
	UserID string    `json:"user_id"`
	Role   auth.Role `json:"role"`
}

// issueTestToken is a test helper so the burst script and reviewers can get
// tokens without an identity provider. It is only routed when
// ENABLE_TOKEN_ENDPOINT=true. In a real system tokens come from the IdP.
func (h *handlers) issueTestToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if !decodeJSON(w, r, &req, defaultBodyLimit) {
		return
	}
	if req.Role == "" {
		req.Role = auth.RoleUser
	}
	errs := fieldErrors{}
	if !auth.ValidUserID(req.UserID) {
		errs.add("user_id", "1-64 characters: letters, digits, '_', '.', '-'")
	}
	if !auth.ValidRole(req.Role) {
		errs.add("role", "must be user or admin")
	}
	if errs.write(w, r) {
		return
	}

	tok, exp, err := h.auth.Issue(req.UserID, req.Role, testTokenTTL)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      tok,
		"token_type": "Bearer",
		"expires_at": exp.UTC(),
	})
}

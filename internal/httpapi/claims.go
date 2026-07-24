package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"signalledger/internal/domain"
)

func (server server) documentClaims(writer http.ResponseWriter, request *http.Request, documentID string) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if server.options.Claims == nil {
		writeError(writer, http.StatusServiceUnavailable, "claims_service_unavailable", "claims service is not configured")
		return
	}
	if !validUUID(documentID) {
		writeError(writer, http.StatusNotFound, "document_not_found", "document was not found")
		return
	}

	claims, err := server.options.Claims.ListClaimsByDocument(request.Context(), documentID)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "document_not_found", "document was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "claims_list_failed", "could not list claims")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"claims": claimResponsesFrom(claims)})
}

func (server server) claimByID(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPatch {
		methodNotAllowed(writer, http.MethodPatch)
		return
	}
	if server.options.Claims == nil {
		writeError(writer, http.StatusServiceUnavailable, "claims_service_unavailable", "claims service is not configured")
		return
	}

	id := strings.TrimPrefix(request.URL.Path, "/v1/claims/")
	if !validUUID(id) {
		writeError(writer, http.StatusNotFound, "claim_not_found", "claim was not found")
		return
	}

	var body struct {
		ValidationStatus string `json:"validation_status"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_body", "request body must be JSON with validation_status")
		return
	}
	if !domain.ValidClaimReviewStatus(body.ValidationStatus) {
		writeError(writer, http.StatusBadRequest, "invalid_validation_status", "validation_status must be accepted or rejected")
		return
	}

	claim, err := server.options.Claims.SetClaimValidationStatus(request.Context(), id, body.ValidationStatus)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "claim_not_found", "claim was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "claim_review_failed", "could not update claim")
		return
	}
	writeJSON(writer, http.StatusOK, claimResponseFrom(claim))
}

type claimResponse struct {
	ID               string    `json:"id"`
	DocumentID       string    `json:"document_id"`
	PageNumber       int       `json:"page_number"`
	Ticker           *string   `json:"ticker,omitempty"`
	ClaimKind        string    `json:"claim_kind"`
	Direction        string    `json:"direction"`
	Claim            string    `json:"claim"`
	EvidenceQuote    string    `json:"evidence_quote"`
	HorizonDays      *int      `json:"horizon_days,omitempty"`
	Confidence       float64   `json:"confidence"`
	EffectiveAt      time.Time `json:"effective_at"`
	ValidationStatus string    `json:"validation_status"`
}

func claimResponseFrom(claim domain.StoredClaim) claimResponse {
	return claimResponse{
		ID:               claim.ID,
		DocumentID:       claim.DocumentID,
		PageNumber:       claim.PageNumber,
		Ticker:           claim.Ticker,
		ClaimKind:        claim.Kind,
		Direction:        claim.Direction,
		Claim:            claim.Text,
		EvidenceQuote:    claim.EvidenceQuote,
		HorizonDays:      claim.HorizonDays,
		Confidence:       claim.Confidence,
		EffectiveAt:      claim.EffectiveAt,
		ValidationStatus: claim.ValidationStatus,
	}
}

func claimResponsesFrom(claims []domain.StoredClaim) []claimResponse {
	responses := make([]claimResponse, 0, len(claims))
	for _, claim := range claims {
		responses = append(responses, claimResponseFrom(claim))
	}
	return responses
}

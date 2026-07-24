package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"signalledger/internal/domain"
	"signalledger/internal/strategies"
)

func (server server) strategies(writer http.ResponseWriter, request *http.Request) {
	if server.options.Strategies == nil {
		writeError(writer, http.StatusServiceUnavailable, "strategy_service_unavailable", "strategy service is not configured")
		return
	}

	switch request.Method {
	case http.MethodPost:
		server.createStrategy(writer, request)
	case http.MethodGet:
		server.listStrategies(writer, request)
	default:
		methodNotAllowed(writer, "GET, POST")
	}
}

func (server server) strategySubpath(writer http.ResponseWriter, request *http.Request) {
	if server.options.Strategies == nil {
		writeError(writer, http.StatusServiceUnavailable, "strategy_service_unavailable", "strategy service is not configured")
		return
	}

	remainder := strings.TrimPrefix(request.URL.Path, "/v1/strategies/")
	if remainder == "draft" {
		server.draftStrategy(writer, request)
		return
	}
	server.strategyByID(writer, request, remainder)
}

func (server server) createStrategy(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Spec     strategies.Spec `json:"spec"`
		ClaimIDs []string        `json:"claim_ids"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_body", "request body must be JSON with spec and claim_ids")
		return
	}
	for _, id := range body.ClaimIDs {
		if !validUUID(id) {
			writeError(writer, http.StatusBadRequest, "invalid_claim_id", "claim_ids must be UUIDs")
			return
		}
	}

	strategy, err := server.options.Strategies.Create(request.Context(), strategies.CreateInput{
		Spec:     body.Spec,
		ClaimIDs: body.ClaimIDs,
	})
	switch {
	case err == nil:
		writeJSON(writer, http.StatusCreated, strategyResponseFrom(strategy, nil))
	case errors.Is(err, strategies.ErrNoClaims):
		writeError(writer, http.StatusBadRequest, "missing_claims", "claim_ids must cite at least one accepted claim")
	case errors.Is(err, strategies.ErrInvalidSpec):
		writeError(writer, http.StatusUnprocessableEntity, "invalid_spec", err.Error())
	case errors.Is(err, domain.ErrClaimNotCitable):
		writeError(writer, http.StatusConflict, "claim_not_citable", "every cited claim must exist and be accepted")
	default:
		writeError(writer, http.StatusInternalServerError, "strategy_create_failed", "could not create strategy")
	}
}

func (server server) listStrategies(writer http.ResponseWriter, request *http.Request) {
	list, err := server.options.Strategies.List(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "strategy_list_failed", "could not list strategies")
		return
	}
	responses := make([]strategyResponse, 0, len(list))
	for _, strategy := range list {
		responses = append(responses, strategyResponseFrom(strategy, nil))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"strategies": responses})
}

func (server server) draftStrategy(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}

	var body struct {
		ClaimIDs    []string `json:"claim_ids"`
		DocumentIDs []string `json:"document_ids"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_body", "request body must be JSON with claim_ids or document_ids")
		return
	}
	for _, id := range append(append([]string{}, body.ClaimIDs...), body.DocumentIDs...) {
		if !validUUID(id) {
			writeError(writer, http.StatusBadRequest, "invalid_id", "claim_ids and document_ids must be UUIDs")
			return
		}
	}

	draft, err := server.options.Strategies.Draft(request.Context(), strategies.DraftInput{
		ClaimIDs:    body.ClaimIDs,
		DocumentIDs: body.DocumentIDs,
	})
	switch {
	case err == nil:
		writeJSON(writer, http.StatusOK, map[string]any{
			"spec":   draft.Spec,
			"claims": claimResponsesFrom(draft.Claims),
		})
	case errors.Is(err, strategies.ErrNoClaims):
		writeError(writer, http.StatusBadRequest, "missing_input", "provide claim_ids or document_ids")
	case errors.Is(err, strategies.ErrClaimMissing), errors.Is(err, domain.ErrNotFound):
		writeError(writer, http.StatusNotFound, "claims_not_found", "one or more referenced records were not found")
	case errors.Is(err, strategies.ErrEmptyDraft):
		writeError(writer, http.StatusUnprocessableEntity, "empty_draft", err.Error())
	default:
		writeError(writer, http.StatusInternalServerError, "strategy_draft_failed", "could not build strategy draft")
	}
}

func (server server) strategyByID(writer http.ResponseWriter, request *http.Request, id string) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !validUUID(id) {
		writeError(writer, http.StatusNotFound, "strategy_not_found", "strategy was not found")
		return
	}

	strategy, citations, err := server.options.Strategies.Get(request.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "strategy_not_found", "strategy was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "strategy_lookup_failed", "could not load strategy")
		return
	}
	writeJSON(writer, http.StatusOK, strategyResponseFrom(strategy, citations))
}

type strategyResponse struct {
	ID        string          `json:"id"`
	Slug      string          `json:"slug"`
	Version   int             `json:"version"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	CreatedAt time.Time       `json:"created_at"`
	Citations []claimResponse `json:"citations,omitempty"`
}

func strategyResponseFrom(strategy domain.Strategy, citations []domain.StoredClaim) strategyResponse {
	response := strategyResponse{
		ID:        strategy.ID,
		Slug:      strategy.Slug,
		Version:   strategy.Version,
		Name:      strategy.Name,
		Spec:      strategy.Spec,
		CreatedAt: strategy.CreatedAt,
	}
	if len(citations) > 0 {
		response.Citations = claimResponsesFrom(citations)
	}
	return response
}

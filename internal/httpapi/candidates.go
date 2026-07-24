package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"signalledger/internal/domain"
)

type CandidateService interface {
	ListCandidates(context.Context, domain.CandidateFilter) ([]domain.Candidate, error)
}

// candidates serves the ranked positions of completed backtest runs. Each one
// carries the run that produced it and the page-cited claims behind it, so a
// reader can trace any position back to a sentence in a PDF.
func (server server) candidates(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if server.options.Candidates == nil {
		writeError(writer, http.StatusServiceUnavailable, "candidate_service_unavailable", "candidate service is not configured")
		return
	}

	query := request.URL.Query()
	filter := domain.CandidateFilter{
		StrategyID: strings.TrimSpace(query.Get("strategy_id")),
		BacktestID: strings.TrimSpace(query.Get("backtest_id")),
	}
	if filter.StrategyID != "" && !validUUID(filter.StrategyID) {
		writeError(writer, http.StatusBadRequest, "invalid_strategy_id", "strategy_id must be a UUID")
		return
	}
	if filter.BacktestID != "" && !validUUID(filter.BacktestID) {
		writeError(writer, http.StatusBadRequest, "invalid_backtest_id", "backtest_id must be a UUID")
		return
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			writeError(writer, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		filter.Limit = limit
	}

	candidates, err := server.options.Candidates.ListCandidates(request.Context(), filter)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "candidate_list_failed", "could not list candidates")
		return
	}

	responses := make([]candidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		responses = append(responses, candidateResponseFrom(candidate))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"candidates": responses})
}

type candidateResponse struct {
	ID              string                      `json:"id"`
	Symbol          string                      `json:"symbol"`
	Rank            int                         `json:"rank"`
	Weight          float64                     `json:"weight"`
	Score           float64                     `json:"score"`
	Momentum        float64                     `json:"momentum"`
	ClaimSupport    float64                     `json:"claim_support"`
	AsOf            string                      `json:"as_of"`
	StrategyID      string                      `json:"strategy_id"`
	StrategySlug    string                      `json:"strategy_slug"`
	StrategyVersion int                         `json:"strategy_version"`
	BacktestRunID   string                      `json:"backtest_run_id"`
	EngineVersion   *string                     `json:"engine_version,omitempty"`
	ResultChecksum  *string                     `json:"result_checksum,omitempty"`
	Evidence        []candidateEvidenceResponse `json:"evidence"`
}

// candidateEvidenceResponse is the whole point of the endpoint: a position
// answers "why?" with a page number and the sentence it was drawn from.
type candidateEvidenceResponse struct {
	ClaimID       string    `json:"claim_id"`
	DocumentID    string    `json:"document_id"`
	PageNumber    int       `json:"page_number"`
	Claim         string    `json:"claim"`
	EvidenceQuote string    `json:"evidence_quote"`
	Direction     string    `json:"direction"`
	Confidence    float64   `json:"confidence"`
	Contribution  float64   `json:"contribution"`
	EffectiveAt   time.Time `json:"effective_at"`
}

func candidateResponseFrom(candidate domain.Candidate) candidateResponse {
	evidence := make([]candidateEvidenceResponse, 0, len(candidate.Evidence))
	for _, item := range candidate.Evidence {
		evidence = append(evidence, candidateEvidenceResponse{
			ClaimID:       item.Claim.ID,
			DocumentID:    item.Claim.DocumentID,
			PageNumber:    item.Claim.PageNumber,
			Claim:         item.Claim.Text,
			EvidenceQuote: item.Claim.EvidenceQuote,
			Direction:     item.Claim.Direction,
			Confidence:    item.Claim.Confidence,
			Contribution:  item.Contribution,
			EffectiveAt:   item.Claim.EffectiveAt,
		})
	}
	return candidateResponse{
		ID:              candidate.ID,
		Symbol:          candidate.Symbol,
		Rank:            candidate.Rank,
		Weight:          candidate.Weight,
		Score:           candidate.Score,
		Momentum:        candidate.Momentum,
		ClaimSupport:    candidate.ClaimSupport,
		AsOf:            candidate.AsOf.Format(time.DateOnly),
		StrategyID:      candidate.StrategyID,
		StrategySlug:    candidate.StrategySlug,
		StrategyVersion: candidate.StrategyVersion,
		BacktestRunID:   candidate.BacktestRunID,
		EngineVersion:   candidate.EngineVersion,
		ResultChecksum:  candidate.ResultChecksum,
		Evidence:        evidence,
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"signalledger/internal/domain"
)

const testBacktestID = "16fd2706-8baf-433b-82eb-8c7fada847da"

type fakeCandidateService struct {
	candidates []domain.Candidate
	filter     domain.CandidateFilter
	err        error
}

func (service *fakeCandidateService) ListCandidates(_ context.Context, filter domain.CandidateFilter) ([]domain.Candidate, error) {
	service.filter = filter
	return service.candidates, service.err
}

func testCandidate() domain.Candidate {
	engineVersion := "momentum-claims-v2"
	return domain.Candidate{
		ID:              "b1f4a2c0-0000-4000-8000-000000000001",
		BacktestRunID:   testBacktestID,
		StrategyID:      testStrategyID,
		StrategySlug:    "macro-oil",
		StrategyVersion: 2,
		EngineVersion:   &engineVersion,
		AsOf:            time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Symbol:          "XLE",
		Rank:            1,
		Weight:          0.2,
		Score:           0.31,
		Momentum:        0.24,
		ClaimSupport:    0.7,
		Evidence: []domain.CandidateClaim{
			{Claim: storedClaim("accepted"), Contribution: 0.7},
		},
	}
}

func TestListCandidatesReturnsEvidence(t *testing.T) {
	t.Parallel()

	service := &fakeCandidateService{candidates: []domain.Candidate{testCandidate()}}
	handler := NewHandler(Options{Candidates: service})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/candidates", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Candidates []candidateResponse `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("candidates = %+v", body.Candidates)
	}

	candidate := body.Candidates[0]
	if candidate.Symbol != "XLE" || candidate.Rank != 1 || candidate.AsOf != "2026-03-02" {
		t.Fatalf("candidate = %+v", candidate)
	}
	// A position without a traceable page is the failure this endpoint exists to
	// prevent.
	if len(candidate.Evidence) != 1 {
		t.Fatalf("evidence = %+v", candidate.Evidence)
	}
	if candidate.Evidence[0].PageNumber != 3 || candidate.Evidence[0].Contribution != 0.7 {
		t.Fatalf("evidence = %+v", candidate.Evidence[0])
	}
}

func TestListCandidatesPassesFilters(t *testing.T) {
	t.Parallel()

	service := &fakeCandidateService{}
	handler := NewHandler(Options{Candidates: service})
	target := fmt.Sprintf("/v1/candidates?strategy_id=%s&backtest_id=%s&limit=5", testStrategyID, testBacktestID)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	expected := domain.CandidateFilter{StrategyID: testStrategyID, BacktestID: testBacktestID, Limit: 5}
	if service.filter != expected {
		t.Fatalf("filter = %+v, want %+v", service.filter, expected)
	}
}

func TestListCandidatesRejectsBadQuery(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"bad strategy id": "/v1/candidates?strategy_id=nope",
		"bad backtest id": "/v1/candidates?backtest_id=nope",
		"bad limit":       "/v1/candidates?limit=0",
	}

	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler := NewHandler(Options{Candidates: &fakeCandidateService{}})
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestListCandidatesWithoutServiceIsUnavailable(t *testing.T) {
	t.Parallel()

	handler := NewHandler(Options{})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/candidates", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"signalledger/internal/domain"
	"signalledger/internal/marketdata"
	"signalledger/internal/strategies"
)

const (
	testDocumentID = "f1c57343-769c-4f85-9f27-53790c7c4e8a"
	testClaimID    = "0f8fad5b-d9cb-469f-a165-70867728950e"
	testStrategyID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

func storedClaim(status string) domain.StoredClaim {
	return domain.StoredClaim{
		ID:               testClaimID,
		DocumentID:       testDocumentID,
		PageNumber:       3,
		Kind:             "macro",
		Direction:        "positive",
		Text:             "Oil demand should improve.",
		EvidenceQuote:    "Oil demand should improve.",
		Confidence:       0.7,
		EffectiveAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidationStatus: status,
	}
}

type fakeClaimsService struct {
	claims []domain.StoredClaim
	err    error
}

func (service *fakeClaimsService) ListClaimsByDocument(_ context.Context, _ string) ([]domain.StoredClaim, error) {
	return service.claims, service.err
}

func (service *fakeClaimsService) SetClaimValidationStatus(_ context.Context, _ string, status string) (domain.StoredClaim, error) {
	if service.err != nil {
		return domain.StoredClaim{}, service.err
	}
	return storedClaim(status), nil
}

func TestListDocumentClaims(t *testing.T) {
	t.Parallel()

	handler := NewHandler(Options{Claims: &fakeClaimsService{claims: []domain.StoredClaim{storedClaim("pending")}}})
	request := httptest.NewRequest(http.MethodGet, "/v1/documents/"+testDocumentID+"/claims", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Claims []claimResponse `json:"claims"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Claims) != 1 || body.Claims[0].PageNumber != 3 {
		t.Fatalf("claims = %+v", body.Claims)
	}
}

func TestReviewClaim(t *testing.T) {
	t.Parallel()

	handler := NewHandler(Options{Claims: &fakeClaimsService{}})
	request := httptest.NewRequest(http.MethodPatch, "/v1/claims/"+testClaimID,
		strings.NewReader(`{"validation_status":"accepted"}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var body claimResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ValidationStatus != "accepted" {
		t.Fatalf("validation status = %q", body.ValidationStatus)
	}
}

func TestReviewClaimRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	handler := NewHandler(Options{Claims: &fakeClaimsService{}})
	request := httptest.NewRequest(http.MethodPatch, "/v1/claims/"+testClaimID,
		strings.NewReader(`{"validation_status":"maybe"}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
}

type fakeStrategyService struct {
	draft    strategies.Draft
	strategy domain.Strategy
	err      error
}

func (service *fakeStrategyService) Draft(_ context.Context, _ strategies.DraftInput) (strategies.Draft, error) {
	return service.draft, service.err
}

func (service *fakeStrategyService) Create(_ context.Context, _ strategies.CreateInput) (domain.Strategy, error) {
	return service.strategy, service.err
}

func (service *fakeStrategyService) List(_ context.Context) ([]domain.Strategy, error) {
	return []domain.Strategy{service.strategy}, service.err
}

func (service *fakeStrategyService) Get(_ context.Context, _ string) (domain.Strategy, []domain.StoredClaim, error) {
	return service.strategy, []domain.StoredClaim{storedClaim("accepted")}, service.err
}

func testStrategy() domain.Strategy {
	return domain.Strategy{
		ID:        testStrategyID,
		Slug:      "macro-oil",
		Version:   1,
		Name:      "Macro oil theme",
		Spec:      json.RawMessage(`{"slug":"macro-oil"}`),
		CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
}

func TestDraftStrategy(t *testing.T) {
	t.Parallel()

	service := &fakeStrategyService{draft: strategies.Draft{
		Spec:   strategies.Spec{Slug: "macro-oil", Version: 1},
		Claims: []domain.StoredClaim{storedClaim("accepted")},
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/strategies/draft",
		strings.NewReader(`{"document_ids":["`+testDocumentID+`"]}`))
	recorder := httptest.NewRecorder()

	NewHandler(Options{Strategies: service}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"macro-oil"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestCreateStrategyReturnsCreated(t *testing.T) {
	t.Parallel()

	service := &fakeStrategyService{strategy: testStrategy()}
	request := httptest.NewRequest(http.MethodPost, "/v1/strategies",
		strings.NewReader(`{"spec":{"slug":"macro-oil"},"claim_ids":["`+testClaimID+`"]}`))
	recorder := httptest.NewRecorder()

	NewHandler(Options{Strategies: service}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateStrategyRejectsUncitableClaims(t *testing.T) {
	t.Parallel()

	service := &fakeStrategyService{err: domain.ErrClaimNotCitable}
	request := httptest.NewRequest(http.MethodPost, "/v1/strategies",
		strings.NewReader(`{"spec":{"slug":"macro-oil"},"claim_ids":["`+testClaimID+`"]}`))
	recorder := httptest.NewRecorder()

	NewHandler(Options{Strategies: service}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestGetStrategyIncludesCitations(t *testing.T) {
	t.Parallel()

	service := &fakeStrategyService{strategy: testStrategy()}
	request := httptest.NewRequest(http.MethodGet, "/v1/strategies/"+testStrategyID, nil)
	recorder := httptest.NewRecorder()

	NewHandler(Options{Strategies: service}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"citations"`) {
		t.Fatalf("body missing citations: %s", recorder.Body.String())
	}
}

type fakeMarketDataService struct {
	job      domain.Job
	snapshot domain.MarketDataSnapshot
	err      error
	request  domain.MarketDataRequest
}

func (service *fakeMarketDataService) RequestSnapshot(_ context.Context, request domain.MarketDataRequest) (domain.Job, error) {
	service.request = request
	if service.err != nil {
		return domain.Job{}, service.err
	}
	return service.job, nil
}

func (service *fakeMarketDataService) List(_ context.Context) ([]domain.MarketDataSnapshot, error) {
	return []domain.MarketDataSnapshot{service.snapshot}, service.err
}

func (service *fakeMarketDataService) Get(_ context.Context, _ string) (domain.MarketDataSnapshot, error) {
	return service.snapshot, service.err
}

func TestRequestMarketDataSnapshot(t *testing.T) {
	t.Parallel()

	service := &fakeMarketDataService{job: domain.Job{ID: testClaimID}}
	request := httptest.NewRequest(http.MethodPost, "/v1/market-data/snapshots",
		strings.NewReader(`{"symbols":["USO"],"start_date":"2025-01-01","end_date":"2025-06-30"}`))
	recorder := httptest.NewRecorder()

	NewHandler(Options{MarketData: service}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if service.request.Interval != "1d" {
		t.Fatalf("interval default = %q", service.request.Interval)
	}
}

func TestRequestMarketDataSnapshotRejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	service := &fakeMarketDataService{err: marketdata.ErrInvalidRequest}
	request := httptest.NewRequest(http.MethodPost, "/v1/market-data/snapshots",
		strings.NewReader(`{"symbols":["uso"],"start_date":"2025-01-01","end_date":"2025-06-30"}`))
	recorder := httptest.NewRecorder()

	NewHandler(Options{MarketData: service}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

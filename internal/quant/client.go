package quant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"signalledger/internal/domain"
)

const maxResponseBytes = 20 << 20

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (client *Client) ExtractClaims(ctx context.Context, document domain.Document) (domain.Extraction, error) {
	effectiveAt := document.UploadedAt
	if document.SourcePublishedAt != nil {
		effectiveAt = *document.SourcePublishedAt
	}

	payload, err := json.Marshal(extractionRequest{
		DocumentID:  document.ID,
		StorageKey:  document.StorageKey,
		EffectiveAt: effectiveAt,
	})
	if err != nil {
		return domain.Extraction{}, fmt.Errorf("encode extraction request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/v1/extract-claims", bytes.NewReader(payload))
	if err != nil {
		return domain.Extraction{}, fmt.Errorf("create extraction request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.httpClient.Do(request)
	if err != nil {
		return domain.Extraction{}, fmt.Errorf("call quant extraction service: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return domain.Extraction{}, fmt.Errorf("read extraction response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return domain.Extraction{}, fmt.Errorf("extraction response exceeds size limit")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return domain.Extraction{}, fmt.Errorf("quant extraction service returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	var result extractionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return domain.Extraction{}, fmt.Errorf("decode extraction response: %w", err)
	}

	extraction := domain.Extraction{
		Pages:  make([]domain.Page, 0, len(result.Pages)),
		Claims: make([]domain.Claim, 0, len(result.Claims)),
	}
	for _, page := range result.Pages {
		extraction.Pages = append(extraction.Pages, domain.Page{Number: page.PageNumber, Content: page.Content})
	}
	for _, claim := range result.Claims {
		extraction.Claims = append(extraction.Claims, domain.Claim{
			PageNumber:    claim.PageNumber,
			Ticker:        claim.Ticker,
			Kind:          claim.ClaimKind,
			Direction:     claim.Direction,
			Text:          claim.Claim,
			EvidenceQuote: claim.EvidenceQuote,
			HorizonDays:   claim.HorizonDays,
			Confidence:    claim.Confidence,
			EffectiveAt:   claim.EffectiveAt,
		})
	}
	if err := extraction.Validate(); err != nil {
		return domain.Extraction{}, fmt.Errorf("validate extraction response: %w", err)
	}
	return extraction, nil
}

func (client *Client) FetchMarketData(ctx context.Context, request domain.MarketDataRequest) (domain.MarketDataResult, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return domain.MarketDataResult{}, fmt.Errorf("encode market data request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/v1/market-data", bytes.NewReader(payload))
	if err != nil {
		return domain.MarketDataResult{}, fmt.Errorf("create market data request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return domain.MarketDataResult{}, fmt.Errorf("call quant market data service: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return domain.MarketDataResult{}, fmt.Errorf("read market data response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return domain.MarketDataResult{}, fmt.Errorf("market data response exceeds size limit")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return domain.MarketDataResult{}, fmt.Errorf("quant market data service returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	var result marketDataResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return domain.MarketDataResult{}, fmt.Errorf("decode market data response: %w", err)
	}
	return domain.MarketDataResult{
		Provider:    result.Provider,
		Bars:        result.Bars,
		RetrievedAt: result.RetrievedAt,
	}, nil
}

type marketDataResponse struct {
	Provider    string       `json:"provider"`
	Bars        []domain.Bar `json:"bars"`
	RetrievedAt time.Time    `json:"retrieved_at"`
}

type extractionRequest struct {
	DocumentID  string    `json:"document_id"`
	StorageKey  string    `json:"storage_key"`
	EffectiveAt time.Time `json:"effective_at"`
}

type extractionResponse struct {
	Pages  []extractionPage  `json:"pages"`
	Claims []extractionClaim `json:"claims"`
}

type extractionPage struct {
	PageNumber int    `json:"page_number"`
	Content    string `json:"content"`
}

type extractionClaim struct {
	PageNumber    int       `json:"page_number"`
	Ticker        *string   `json:"ticker"`
	Claim         string    `json:"claim"`
	EvidenceQuote string    `json:"evidence_quote"`
	ClaimKind     string    `json:"claim_kind"`
	Direction     string    `json:"direction"`
	HorizonDays   *int      `json:"horizon_days"`
	Confidence    float64   `json:"confidence"`
	EffectiveAt   time.Time `json:"effective_at"`
}

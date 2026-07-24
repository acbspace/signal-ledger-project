package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"signalledger/internal/domain"
	"signalledger/internal/marketdata"
)

func (server server) marketDataSnapshots(writer http.ResponseWriter, request *http.Request) {
	if server.options.MarketData == nil {
		writeError(writer, http.StatusServiceUnavailable, "market_data_service_unavailable", "market data service is not configured")
		return
	}

	switch request.Method {
	case http.MethodPost:
		server.requestMarketDataSnapshot(writer, request)
	case http.MethodGet:
		server.listMarketDataSnapshots(writer, request)
	default:
		methodNotAllowed(writer, "GET, POST")
	}
}

func (server server) requestMarketDataSnapshot(writer http.ResponseWriter, request *http.Request) {
	var body domain.MarketDataRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_body", "request body must be JSON with symbols, start_date, and end_date")
		return
	}
	if body.Interval == "" {
		body.Interval = "1d"
	}

	job, err := server.options.MarketData.RequestSnapshot(request.Context(), body)
	if errors.Is(err, marketdata.ErrInvalidRequest) {
		writeError(writer, http.StatusBadRequest, "invalid_market_data_request", err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "snapshot_request_failed", "could not enqueue market data snapshot")
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"job_id": job.ID})
}

func (server server) listMarketDataSnapshots(writer http.ResponseWriter, request *http.Request) {
	snapshots, err := server.options.MarketData.List(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "snapshot_list_failed", "could not list market data snapshots")
		return
	}
	responses := make([]snapshotResponse, 0, len(snapshots))
	for _, snapshot := range snapshots {
		responses = append(responses, snapshotResponseFrom(snapshot))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"snapshots": responses})
}

func (server server) marketDataSnapshotByID(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if server.options.MarketData == nil {
		writeError(writer, http.StatusServiceUnavailable, "market_data_service_unavailable", "market data service is not configured")
		return
	}

	id := strings.TrimPrefix(request.URL.Path, "/v1/market-data/snapshots/")
	if !validUUID(id) {
		writeError(writer, http.StatusNotFound, "snapshot_not_found", "market data snapshot was not found")
		return
	}

	snapshot, err := server.options.MarketData.Get(request.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "snapshot_not_found", "market data snapshot was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "snapshot_lookup_failed", "could not load market data snapshot")
		return
	}
	writeJSON(writer, http.StatusOK, snapshotResponseFrom(snapshot))
}

type snapshotResponse struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	Symbols     []string  `json:"symbols"`
	Interval    string    `json:"interval"`
	StartDate   string    `json:"start_date"`
	EndDate     string    `json:"end_date"`
	StorageKey  *string   `json:"storage_key,omitempty"`
	Checksum    *string   `json:"checksum,omitempty"`
	RetrievedAt time.Time `json:"retrieved_at"`
}

func snapshotResponseFrom(snapshot domain.MarketDataSnapshot) snapshotResponse {
	return snapshotResponse{
		ID:          snapshot.ID,
		Provider:    snapshot.Provider,
		Symbols:     snapshot.Symbols,
		Interval:    snapshot.Interval,
		StartDate:   snapshot.StartDate,
		EndDate:     snapshot.EndDate,
		StorageKey:  snapshot.StorageKey,
		Checksum:    snapshot.Checksum,
		RetrievedAt: snapshot.RetrievedAt,
	}
}

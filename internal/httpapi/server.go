package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"signalledger/internal/documents"
	"signalledger/internal/domain"
	"signalledger/internal/strategies"
)

const defaultMaxUploadBytes int64 = 20 << 20

type ClaimsService interface {
	ListClaimsByDocument(context.Context, string) ([]domain.StoredClaim, error)
	SetClaimValidationStatus(context.Context, string, string) (domain.StoredClaim, error)
}

type StrategyService interface {
	Draft(context.Context, strategies.DraftInput) (strategies.Draft, error)
	Create(context.Context, strategies.CreateInput) (domain.Strategy, error)
	List(context.Context) ([]domain.Strategy, error)
	Get(context.Context, string) (domain.Strategy, []domain.StoredClaim, error)
}

type MarketDataService interface {
	RequestSnapshot(context.Context, domain.MarketDataRequest) (domain.Job, error)
	List(context.Context) ([]domain.MarketDataSnapshot, error)
	Get(context.Context, string) (domain.MarketDataSnapshot, error)
}

type Options struct {
	Version        string
	Documents      documents.Uploader
	Claims         ClaimsService
	Strategies     StrategyService
	MarketData     MarketDataService
	Backtests      BacktestStore
	Candidates     CandidateService
	MaxUploadBytes int64
}

type server struct {
	options Options
}

// NewHandler exposes the API boundary. The full pipeline is live: documents,
// claims review, strategies, market-data snapshots, backtests, and the
// evidence-backed candidate rankings those backtests produce.
func NewHandler(options Options) http.Handler {
	application := server{options: options}
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", application.healthz)
	mux.HandleFunc("/v1/documents", application.documents)
	mux.HandleFunc("/v1/documents/", application.documentByID)
	mux.HandleFunc("/v1/claims/", application.claimByID)
	mux.HandleFunc("/v1/strategies", application.strategies)
	mux.HandleFunc("/v1/strategies/", application.strategySubpath)
	mux.HandleFunc("/v1/market-data/snapshots", application.marketDataSnapshots)
	mux.HandleFunc("/v1/market-data/snapshots/", application.marketDataSnapshotByID)
	mux.HandleFunc("/v1/backtests", application.backtests)
	mux.HandleFunc("/v1/backtests/", application.backtestByID)
	mux.HandleFunc("/v1/candidates", application.candidates)

	return withRequestID(mux)
}

func (server server) healthz(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{
		"service": "api",
		"status":  "ok",
		"version": server.options.Version,
	})
}

func (server server) documents(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if server.options.Documents == nil {
		writeError(writer, http.StatusServiceUnavailable, "document_service_unavailable", "document service is not configured")
		return
	}

	maxUploadBytes := server.options.MaxUploadBytes
	if maxUploadBytes <= 0 {
		maxUploadBytes = defaultMaxUploadBytes
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxUploadBytes+(1<<20))
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		if request.MultipartForm != nil {
			defer request.MultipartForm.RemoveAll()
		}
		writeError(writer, uploadParseStatus(err), "invalid_upload", "upload must be a multipart request containing a PDF file")
		return
	}
	defer request.MultipartForm.RemoveAll()

	file, header, err := request.FormFile("file")
	if err != nil {
		writeError(writer, http.StatusBadRequest, "missing_file", "multipart field 'file' is required")
		return
	}
	defer file.Close()
	if header.Filename == "" {
		writeError(writer, http.StatusBadRequest, "invalid_filename", "uploaded file must have a filename")
		return
	}
	if header.Size > maxUploadBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "file_too_large", "uploaded file exceeds the configured size limit")
		return
	}

	publishedAt, err := parsePublishedAt(request.FormValue("published_at"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_published_at", "published_at must use RFC3339 format")
		return
	}

	result, err := server.options.Documents.Upload(request.Context(), documents.UploadInput{
		Filename:          safeFilename(header.Filename),
		MIMEType:          header.Header.Get("Content-Type"),
		SourcePublishedAt: publishedAt,
		Content:           file,
	})
	if err != nil {
		server.writeUploadError(writer, err)
		return
	}

	status := http.StatusAccepted
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(writer, status, documentResponseFrom(result))
}

func (server server) documentByID(writer http.ResponseWriter, request *http.Request) {
	remainder := strings.TrimPrefix(request.URL.Path, "/v1/documents/")
	if id, found := strings.CutSuffix(remainder, "/claims"); found {
		server.documentClaims(writer, request, id)
		return
	}

	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if server.options.Documents == nil {
		writeError(writer, http.StatusServiceUnavailable, "document_service_unavailable", "document service is not configured")
		return
	}

	id := remainder
	if !validUUID(id) {
		writeError(writer, http.StatusNotFound, "document_not_found", "document was not found")
		return
	}
	document, err := server.options.Documents.Get(request.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "document_not_found", "document was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "document_lookup_failed", "could not load document")
		return
	}
	writeJSON(writer, http.StatusOK, documentResponse{
		ID:                document.ID,
		Filename:          document.Filename,
		Status:            document.Status,
		SourcePublishedAt: document.SourcePublishedAt,
		UploadedAt:        document.UploadedAt,
	})
}

func (server server) writeUploadError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, documents.ErrFileTooLarge):
		writeError(writer, http.StatusRequestEntityTooLarge, "file_too_large", "uploaded file exceeds the configured size limit")
	case errors.Is(err, documents.ErrInvalidPDF):
		writeError(writer, http.StatusUnprocessableEntity, "invalid_pdf", "uploaded file is not a readable PDF")
	default:
		writeError(writer, http.StatusInternalServerError, "document_upload_failed", "could not store document")
	}
}

func parsePublishedAt(value string) (*time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func safeFilename(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	value = path.Base(strings.TrimSpace(value))
	if value == "." || value == "/" || value == "" {
		return "document.pdf"
	}
	return value
}

func uploadParseStatus(err error) int {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') && !(character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

func documentResponseFrom(result documents.UploadResult) documentResponse {
	response := documentResponse{
		ID:                result.Document.ID,
		Filename:          result.Document.Filename,
		Status:            result.Document.Status,
		Duplicate:         !result.Created,
		SourcePublishedAt: result.Document.SourcePublishedAt,
		UploadedAt:        result.Document.UploadedAt,
	}
	if result.Job != nil {
		response.JobID = result.Job.ID
	}
	return response
}

type documentResponse struct {
	ID                string     `json:"id"`
	Filename          string     `json:"filename"`
	Status            string     `json:"status"`
	JobID             string     `json:"job_id,omitempty"`
	Duplicate         bool       `json:"duplicate"`
	SourcePublishedAt *time.Time `json:"source_published_at,omitempty"`
	UploadedAt        time.Time  `json:"uploaded_at"`
}

func methodNotAllowed(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"error": code, "message": message})
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = "skeleton-request-id"
		}
		writer.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(writer, request)
	})
}

var _ documents.Uploader = (*documents.Service)(nil)

package httpapi

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"signalledger/internal/documents"
	"signalledger/internal/domain"
)

func TestHealthz(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Options{Version: "test"}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("missing request ID")
	}
}

func TestDocumentUploadReturnsAccepted(t *testing.T) {
	service := &fakeDocumentService{
		result: documents.UploadResult{
			Created: true,
			Document: domain.Document{
				ID:       "f1c57343-769c-4f85-9f27-53790c7c4e8a",
				Filename: "report.pdf",
				Status:   "queued",
				UploadedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			Job: &domain.Job{ID: "0f8fad5b-d9cb-469f-a165-70867728950e"},
		},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "report.pdf")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte("%PDF-1.7\nexample")); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/documents", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()

	NewHandler(Options{Documents: service, MaxUploadBytes: 1024}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := string(service.content); got != "%PDF-1.7\nexample" {
		t.Fatalf("uploaded content = %q", got)
	}
}

func TestDocumentUploadRequiresFile(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/documents", nil)
	recorder := httptest.NewRecorder()

	NewHandler(Options{Documents: &fakeDocumentService{}}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

type fakeDocumentService struct {
	result  documents.UploadResult
	err     error
	content []byte
}

func (service *fakeDocumentService) Upload(_ context.Context, input documents.UploadInput) (documents.UploadResult, error) {
	service.content, _ = io.ReadAll(input.Content)
	return service.result, service.err
}

func (service *fakeDocumentService) Get(_ context.Context, _ string) (domain.Document, error) {
	return service.result.Document, service.err
}

package documents

import (
	"context"
	"io"
	"time"

	"signalledger/internal/domain"
)

type Repository interface {
	CreateDocumentWithExtractionJob(context.Context, domain.CreateDocumentInput) (domain.Document, *domain.Job, bool, error)
	GetDocument(context.Context, string) (domain.Document, error)
}

type Uploader interface {
	Upload(context.Context, UploadInput) (UploadResult, error)
	Get(context.Context, string) (domain.Document, error)
}

type UploadInput struct {
	Filename          string
	MIMEType          string
	SourcePublishedAt *time.Time
	Content           io.Reader
}

type UploadResult struct {
	Document domain.Document
	Job      *domain.Job
	Created  bool
}

type Service struct {
	repository Repository
	files      FileStore
}

func NewService(repository Repository, files FileStore) *Service {
	return &Service{repository: repository, files: files}
}

func (service *Service) Upload(ctx context.Context, input UploadInput) (UploadResult, error) {
	stored, err := service.files.Save(input.Content)
	if err != nil {
		return UploadResult{}, err
	}

	result, err := service.createDocument(ctx, input, stored)
	if err != nil {
		_ = service.files.Delete(stored.StorageKey)
		return UploadResult{}, err
	}
	if !result.Created {
		// The same content was already stored and queued. The duplicate file is
		// not referenced by PostgreSQL and can be safely discarded.
		_ = service.files.Delete(stored.StorageKey)
	}
	return result, nil
}

func (service *Service) Get(ctx context.Context, id string) (domain.Document, error) {
	return service.repository.GetDocument(ctx, id)
}

func (service *Service) createDocument(ctx context.Context, input UploadInput, stored StoredFile) (UploadResult, error) {
	document, job, created, err := service.repository.CreateDocumentWithExtractionJob(ctx, domain.CreateDocumentInput{
		Filename:          input.Filename,
		StorageKey:        stored.StorageKey,
		SHA256:            stored.SHA256,
		SizeBytes:         stored.SizeBytes,
		MIMEType:          input.MIMEType,
		SourcePublishedAt: input.SourcePublishedAt,
	})
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{Document: document, Job: job, Created: created}, nil
}

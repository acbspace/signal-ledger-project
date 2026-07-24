package domain

import (
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("record not found")
	ErrConflict = errors.New("record already exists")
)

type Document struct {
	ID                string
	Filename          string
	StorageKey        string
	SHA256            string
	Status            string
	UploadedAt        time.Time
	SourcePublishedAt *time.Time
}

type CreateDocumentInput struct {
	Filename          string
	StorageKey        string
	SHA256            string
	SizeBytes         int64
	MIMEType          string
	SourcePublishedAt *time.Time
}

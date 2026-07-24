package documents

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreSaveAndDelete(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewFileStore(root, 1024)
	stored, err := store.Save(bytes.NewBufferString("%PDF-1.7\nexample"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if stored.SizeBytes == 0 || stored.SHA256 == "" {
		t.Fatalf("unexpected stored file: %#v", stored)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(stored.StorageKey))); err != nil {
		t.Fatalf("stored path: %v", err)
	}
	if err := store.Delete(stored.StorageKey); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestFileStoreRejectsNonPDF(t *testing.T) {
	t.Parallel()

	store := NewFileStore(t.TempDir(), 1024)
	_, err := store.Save(bytes.NewBufferString("not a PDF"))
	if !errors.Is(err, ErrInvalidPDF) {
		t.Fatalf("error = %v, want ErrInvalidPDF", err)
	}
}

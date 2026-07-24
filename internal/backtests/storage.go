// Package backtests stores backtest run artifacts on the shared volume.
//
// The quant service mounts that volume read-only, so the worker writes the
// artifact it receives. This mirrors marketdata.SnapshotStore: generated storage
// keys, path-escape rejection, and an atomic temp-file rename.
package backtests

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactStore writes equity-curve artifacts beneath Root.
type ArtifactStore struct {
	Root string
}

func NewArtifactStore(root string) ArtifactStore {
	return ArtifactStore{Root: root}
}

// Save writes the artifact and returns its storage key. The caller records the
// checksum computed by the engine over exactly these bytes.
func (store ArtifactStore) Save(content []byte) (string, error) {
	name := make([]byte, 16)
	if _, err := rand.Read(name); err != nil {
		return "", fmt.Errorf("generate backtest storage key: %w", err)
	}
	storageKey := filepath.ToSlash(filepath.Join("backtests", hex.EncodeToString(name)+".csv"))

	destination, err := store.pathFor(storageKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return "", fmt.Errorf("create backtest directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".backtest-*")
	if err != nil {
		return "", fmt.Errorf("create temporary backtest artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("write backtest artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("close backtest artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("store backtest artifact: %w", err)
	}
	return storageKey, nil
}

func (store ArtifactStore) pathFor(storageKey string) (string, error) {
	cleanKey := filepath.Clean(filepath.FromSlash(storageKey))
	if cleanKey == "." || filepath.IsAbs(cleanKey) || strings.HasPrefix(cleanKey, "..") {
		return "", fmt.Errorf("invalid backtest storage key")
	}
	root, err := filepath.Abs(store.Root)
	if err != nil {
		return "", fmt.Errorf("resolve backtest storage root: %w", err)
	}
	return filepath.Join(root, cleanKey), nil
}

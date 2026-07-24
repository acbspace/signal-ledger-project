package marketdata

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"signalledger/internal/domain"
)

// CanonicalCSV serializes bars deterministically: fixed header, rows sorted by
// (symbol, date), shortest round-trip float formatting. The snapshot checksum
// is defined over exactly these bytes so identical data always matches.
func CanonicalCSV(bars []domain.Bar) []byte {
	sorted := make([]domain.Bar, len(bars))
	copy(sorted, bars)
	sort.Slice(sorted, func(left, right int) bool {
		if sorted[left].Symbol != sorted[right].Symbol {
			return sorted[left].Symbol < sorted[right].Symbol
		}
		return sorted[left].Date < sorted[right].Date
	})

	var builder strings.Builder
	builder.WriteString("symbol,date,open,high,low,close,adj_close,volume\n")
	for _, bar := range sorted {
		builder.WriteString(strings.Join([]string{
			bar.Symbol,
			bar.Date,
			formatPrice(bar.Open),
			formatPrice(bar.High),
			formatPrice(bar.Low),
			formatPrice(bar.Close),
			formatPrice(bar.AdjClose),
			strconv.FormatInt(bar.Volume, 10),
		}, ","))
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func Checksum(csv []byte) string {
	sum := sha256.Sum256(csv)
	return hex.EncodeToString(sum[:])
}

func formatPrice(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// SnapshotStore writes snapshot CSVs beneath Root using generated storage keys,
// mirroring the safety rules of the document FileStore.
type SnapshotStore struct {
	Root string
}

func NewSnapshotStore(root string) SnapshotStore {
	return SnapshotStore{Root: root}
}

func (store SnapshotStore) Save(csv []byte) (string, error) {
	name := make([]byte, 16)
	if _, err := rand.Read(name); err != nil {
		return "", fmt.Errorf("generate snapshot storage key: %w", err)
	}
	storageKey := filepath.ToSlash(filepath.Join("market-data", hex.EncodeToString(name)+".csv"))

	destination, err := store.pathFor(storageKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".snapshot-*")
	if err != nil {
		return "", fmt.Errorf("create temporary snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(csv); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("write snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("close snapshot: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("store snapshot: %w", err)
	}
	return storageKey, nil
}

func (store SnapshotStore) pathFor(storageKey string) (string, error) {
	cleanKey := filepath.Clean(filepath.FromSlash(storageKey))
	if cleanKey == "." || filepath.IsAbs(cleanKey) || strings.HasPrefix(cleanKey, "..") {
		return "", fmt.Errorf("invalid snapshot storage key")
	}
	root, err := filepath.Abs(store.Root)
	if err != nil {
		return "", fmt.Errorf("resolve snapshot storage root: %w", err)
	}
	return filepath.Join(root, cleanKey), nil
}

package marketdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signalledger/internal/domain"
)

func sampleBars() []domain.Bar {
	return []domain.Bar{
		{Symbol: "XLE", Date: "2026-01-03", Open: 90.5, High: 91, Low: 90, Close: 90.75, AdjClose: 90.75, Volume: 1200},
		{Symbol: "USO", Date: "2026-01-03", Open: 70.1, High: 71, Low: 69.9, Close: 70.5, AdjClose: 70.5, Volume: 3400},
		{Symbol: "USO", Date: "2026-01-02", Open: 69, High: 70, Low: 68.5, Close: 69.75, AdjClose: 69.75, Volume: 3100},
	}
}

func TestCanonicalCSVSortsAndFormatsDeterministically(t *testing.T) {
	t.Parallel()

	csv := string(CanonicalCSV(sampleBars()))
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if lines[0] != "symbol,date,open,high,low,close,adj_close,volume" {
		t.Fatalf("header = %q", lines[0])
	}
	if lines[1] != "USO,2026-01-02,69,70,68.5,69.75,69.75,3100" {
		t.Fatalf("first row = %q", lines[1])
	}
	if lines[3] != "XLE,2026-01-03,90.5,91,90,90.75,90.75,1200" {
		t.Fatalf("last row = %q", lines[3])
	}

	shuffled := []domain.Bar{sampleBars()[2], sampleBars()[0], sampleBars()[1]}
	if Checksum(CanonicalCSV(sampleBars())) != Checksum(CanonicalCSV(shuffled)) {
		t.Fatal("checksum depends on input order")
	}
}

func TestSnapshotStoreWritesBeneathRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storageKey, err := NewSnapshotStore(root).Save([]byte("symbol,date\n"))
	if err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if !strings.HasPrefix(storageKey, "market-data/") {
		t.Fatalf("storage key = %q", storageKey)
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(storageKey)))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(content) != "symbol,date\n" {
		t.Fatalf("content = %q", content)
	}
}

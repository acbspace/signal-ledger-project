package backtests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactStoreWritesBeneathRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := []byte("date,equity\n2026-03-02,1.00000000\n")

	storageKey, err := NewArtifactStore(root).Save(content)
	if err != nil {
		t.Fatalf("save artifact: %v", err)
	}
	if !strings.HasPrefix(storageKey, "backtests/") || !strings.HasSuffix(storageKey, ".csv") {
		t.Fatalf("storage key = %q", storageKey)
	}

	stored, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(storageKey)))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(stored) != string(content) {
		t.Fatalf("content = %q", stored)
	}
}

func TestArtifactStoreGeneratesDistinctKeys(t *testing.T) {
	t.Parallel()

	store := NewArtifactStore(t.TempDir())
	first, err := store.Save([]byte("a"))
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	second, err := store.Save([]byte("b"))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if first == second {
		t.Fatalf("two saves shared a storage key %q", first)
	}
}

func TestArtifactStoreRejectsTraversalKeys(t *testing.T) {
	t.Parallel()

	store := NewArtifactStore(t.TempDir())
	// Parent-directory traversal is an escape on every platform and must be
	// rejected outright, not merely confined.
	for _, key := range []string{"../escape.csv", "backtests/../../escape.csv"} {
		if _, err := store.pathFor(key); err == nil {
			t.Fatalf("traversal key %q was accepted", key)
		}
	}
}

func TestArtifactStoreConfinesEveryKeyToRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	store := NewArtifactStore(root)

	// The security property that holds across platforms: a hostile key is either
	// rejected, or resolves to a path still under the mounted volume. (An
	// absolute POSIX path is rejected on Linux but treated as relative on
	// Windows, so asserting rejection alone would be platform-dependent.)
	for _, key := range []string{"../x", "a/../../x", "/etc/passwd", `\\server\share\x`, "backtests/ok.csv"} {
		path, err := store.pathFor(key)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(absRoot, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("key %q resolved outside root: %q", key, path)
		}
	}
}

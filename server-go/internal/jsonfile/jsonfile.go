// Package jsonfile provides the read / atomic-write / mtime semantics every JSON document
// under data/ shares.
//
// Centralised because several packages persist their own state there (sessions, API keys,
// the GitHub cache, the usage rollup, the AI-credit snapshot) and writing the
// tmp-then-rename dance in each of them is a divergence waiting to happen.
package jsonfile

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/satomic/model-router/server-go/internal/omap"
)

// Read returns the document at path, or a fresh empty map when it is missing or corrupt.
// A corrupt file must not block startup, so a parse failure reads as "nothing there".
func Read(path string) *omap.Map {
	data, err := os.ReadFile(path)
	if err != nil {
		return omap.New()
	}
	value, err := omap.FromJSON(data)
	if err != nil {
		return omap.New()
	}
	if m, ok := value.(*omap.Map); ok {
		return m
	}
	return omap.New()
}

// Write persists data through a temporary file and an atomic rename, so a concurrent
// reader never sees half a document.
func Write(path string, data any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MTime returns the modification time in seconds, or 0 when the file is unreadable.
// Used as a cheap "has another worker written this" check before re-reading.
func MTime(path string) float64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(info.ModTime().UnixNano()) / 1e9
}

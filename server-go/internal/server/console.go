package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satomic/model-router/server-go/internal/web"
)

// serveConsole hands over the built single-page console.
//
// Registered last, so every concrete API route still wins. This is what lets a real URL such as
// /config/models survive a reload or a paste into somebody else's browser.
func (a *App) serveConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"}, nil)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")

	if !a.hasConsole() {
		// No console was bundled. The API still works; the root is the only thing that has
		// nothing to show.
		if path == "" {
			writeJSON(w, http.StatusOK, mapOf(
				"detail", "the console was not bundled into this build; the API is available under /v1",
			), nil)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "not found"}, nil)
		return
	}

	if path == "" {
		a.serveIndex(w, r)
		return
	}
	// An unknown path underneath an API prefix has to keep 404ing as JSON -- answering it with
	// the console shell would turn every client typo into a 200 page of HTML that a fetch() then
	// fails to parse.
	if path == "ui" {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "not found"}, nil)
		return
	}
	for _, prefix := range reserved {
		if strings.HasPrefix(path, prefix) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "not found"}, nil)
			return
		}
	}

	if content, modTime, ok := a.openAsset(path); ok {
		http.ServeContent(w, r, filepath.Base(path), modTime, content)
		content.Close()
		return
	}
	// An extension in the last segment means a missing *file*, not a client route: 404 it, so a
	// stale build asking for a bundle that no longer exists fails loudly instead of receiving
	// index.html under a JavaScript content type.
	if strings.Contains(filepath.Base(path), ".") {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "not found"}, nil)
		return
	}
	// An unknown application path: hand over the shell and let the router render its own
	// not-found view. The server has no list of client routes to check against.
	a.serveIndex(w, r)
}

func (a *App) hasConsole() bool {
	if a.dist != "" {
		if _, err := os.Stat(filepath.Join(a.dist, "index.html")); err == nil {
			return true
		}
	}
	return web.HasEmbedded()
}

func (a *App) serveIndex(w http.ResponseWriter, r *http.Request) {
	if content, modTime, ok := a.openAsset("index.html"); ok {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", modTime, content)
		content.Close()
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"detail": "not found"}, nil)
}

// openAsset resolves one console file, preferring the on-disk build when there is one.
//
// On-disk first so a `npm run build` is picked up without rebuilding the binary; the embedded
// copy is what makes a standalone binary work with no frontend/ directory beside it.
func (a *App) openAsset(name string) (readSeekCloser, modTimeValue, bool) {
	if a.dist != "" {
		candidate := filepath.Join(a.dist, filepath.FromSlash(name))
		// Refuse anything that escapes the build directory: the path comes from the URL.
		if resolved, err := filepath.Abs(candidate); err == nil {
			root, err := filepath.Abs(a.dist)
			if err == nil && (resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator))) {
				if file, err := os.Open(resolved); err == nil {
					if info, err := file.Stat(); err == nil && !info.IsDir() {
						return file, info.ModTime(), true
					}
					file.Close()
				}
			}
		}
	}
	return web.Open(name)
}

// Local aliases, so this file reads without repeating the web package's signature.
type readSeekCloser = web.ReadSeekCloser
type modTimeValue = time.Time

// Package web carries the built console, embedded into the binary.
//
// The files land here at build time (see scripts/build.sh, which copies frontend/dist into
// internal/web/dist), so a released binary serves the console with no frontend/ directory
// beside it. When the tree does have a build on disk, the server prefers that one -- a
// `npm run build` then shows up without recompiling Go.
package web

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"time"
)

//go:embed all:dist
var dist embed.FS

// HasEmbedded reports whether a console was bundled into this binary.
func HasEmbedded() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// ReadSeekCloser is what http.ServeContent needs, plus the Close the caller owns.
type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

type memoryFile struct{ *bytes.Reader }

func (memoryFile) Close() error { return nil }

// buildTime stamps every embedded asset. A constant rather than the real build time: embed
// records no timestamps, and a zero time makes http.ServeContent skip Last-Modified entirely,
// which is the honest answer for a file whose age it cannot know.
var buildTime = time.Time{}

// Open resolves one embedded file.
func Open(name string) (ReadSeekCloser, time.Time, bool) {
	data, err := dist.ReadFile("dist/" + name)
	if err != nil {
		return nil, buildTime, false
	}
	return memoryFile{bytes.NewReader(data)}, buildTime, true
}

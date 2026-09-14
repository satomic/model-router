package server

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Access logging.
//
// The Python backend runs under uvicorn, which writes a line per request for free. This server
// is its own HTTP stack, so without this it logs only the handful of events that call logInfo --
// and browsing the console, or getting a 500 out of it, produces nothing at all in `docker logs`.
// That is a bad way to run a gateway: the first question about any incident is "did the request
// even arrive", and an empty log cannot answer it.
//
// One line per request, carrying what uvicorn's line carries plus the duration, which for a
// router is the number worth having.

// accessLogEnabled reads MR_ACCESS_LOG. On by default, because the Python backend logs requests
// and an operator switching backends should not have to discover that the new one went quiet.
// Set it to "off"/"0"/"false" on a deployment that takes its access log from a proxy in front.
func accessLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MR_ACCESS_LOG"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// recorder captures the status and the byte count on the way past.
//
// It has to stay transparent to everything the handlers below rely on: Flush above all, because
// an SSE relay that cannot flush would buffer a streamed answer until the model finished --
// turning the feature this router exists for into a plain request/response. Unwrap keeps
// http.ResponseController working for anything that reaches for it later.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withAccessLog writes one line per request, after the response has been written.
//
// After, not before, so the line carries the outcome: a log that only records arrivals cannot
// distinguish a request that 500ed from one still streaming. For a streamed response the
// duration is therefore the whole stream, which is the honest figure -- that is how long the
// caller waited.
func withAccessLog(next http.Handler) http.Handler {
	if !accessLogEnabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			// A handler that wrote nothing at all: net/http sends 200 with an empty body.
			status = http.StatusOK
		}
		target := r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		logInfo("%s %q %d %d %s",
			clientAddr(r), r.Method+" "+target+" "+r.Proto, status, rec.bytes,
			formatDuration(time.Since(start)))
	})
}

// clientAddr is who to blame for the request. X-Forwarded-For first, because behind a proxy the
// socket address is the proxy on every line and the log stops being able to tell callers apart.
func clientAddr(r *http.Request) string {
	if forwarded := r.Header.Get("x-forwarded-for"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// formatDuration keeps the unit fixed at milliseconds, so the column stays sortable by eye --
// Go's own formatting would switch between µs, ms and s from line to line.
func formatDuration(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 1, 64) + "ms"
}

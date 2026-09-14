package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecorderStaysFlushable is the regression this wrapper could most easily cause: the SSE
// relay reaches for http.Flusher, and a ResponseWriter that hides it would buffer a streamed
// answer until the model finished -- turning the feature this router exists for into a plain
// request/response, with nothing failing loudly to say so.
func TestRecorderStaysFlushable(t *testing.T) {
	var flushed int
	handler := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the access-log wrapper hid http.Flusher from the handler")
			return
		}
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: chunk\n\n"))
			flusher.Flush()
			flushed++
		}
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if flushed != 3 {
		t.Errorf("flushed %d times, want 3", flushed)
	}
	if body := rec.Body.String(); strings.Count(body, "data: chunk") != 3 {
		t.Errorf("body = %q, want three chunks", body)
	}
}

// TestRecorderReportsTheStatusTheHandlerSet: a line that says 200 for a request that 500ed is
// worse than no line at all.
func TestRecorderReportsTheStatus(t *testing.T) {
	cases := []struct {
		name   string
		handle func(http.ResponseWriter)
		want   int
	}{
		{"explicit", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, http.StatusBadGateway},
		// A handler that writes a body without a WriteHeader gets an implicit 200.
		{"implicit", func(w http.ResponseWriter) { _, _ = w.Write([]byte("ok")) }, http.StatusOK},
		// And one that writes nothing at all still ends as 200, which is what net/http sends.
		{"silent", func(w http.ResponseWriter) {}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{ResponseWriter: httptest.NewRecorder()}
			c.handle(rec)
			got := rec.status
			if got == 0 {
				got = http.StatusOK // what withAccessLog substitutes
			}
			if got != c.want {
				t.Errorf("status = %d, want %d", got, c.want)
			}
		})
	}
}

// TestClientAddrPrefersTheForwardedCaller: behind a proxy the socket address is the proxy on
// every line, and the log stops being able to tell callers apart.
func TestClientAddrPrefersTheForwardedCaller(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	if got := clientAddr(r); got != "10.0.0.9" {
		t.Errorf("clientAddr = %q, want the socket host", got)
	}
	r.Header.Set("x-forwarded-for", "203.0.113.7, 10.0.0.1")
	if got := clientAddr(r); got != "203.0.113.7" {
		t.Errorf("clientAddr = %q, want the original caller", got)
	}
}

func TestAccessLogCanBeTurnedOff(t *testing.T) {
	t.Setenv("MR_ACCESS_LOG", "off")
	if accessLogEnabled() {
		t.Error("MR_ACCESS_LOG=off did not disable the access log")
	}
	t.Setenv("MR_ACCESS_LOG", "")
	if !accessLogEnabled() {
		t.Error("the access log should be on by default")
	}
}

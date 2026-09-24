package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/satomic/model-router/server-go/internal/omap"
)

// typesafeHTTP is the client for TypeSafe's System One API. It sits here, next to the model
// clients, only to share their transport: the decision call is on the latency path of every
// AI-routed request, and a warm TLS connection to api.typesafe.ai is most of what makes a
// ~100ms decision possible. The per-call deadline comes from the caller's context
// (ai_router.timeout_seconds), not from a client-wide timeout.
var typesafeHTTP = &http.Client{Transport: sharedTransport}

// TypeSafeSystemOne posts one request to `<baseURL>/v1/systemone` and returns the decoded body.
//
// Not a pooled Client: there is no protocol to translate and no stream, just one bearer-token
// POST whose request and response are already JSON maps.
func TypeSafeSystemOne(ctx context.Context, baseURL, apiKey string, body any) (*omap.Map, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/systemone", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := typesafeHTTP.Do(req)
	if err != nil {
		return nil, explainDNS(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// TypeSafe's envelope is {"detail": {"error_type", "message"}} (a 422 may carry a list
		// instead). Unwrapped here so the trace reads "401: Cannot authenticate..." rather than
		// a JSON blob; anything else goes through the shared parser.
		if value, err := omap.FromJSON(payload); err == nil {
			if data, ok := value.(*omap.Map); ok {
				if msg := data.Map("detail").Str("message"); msg != "" {
					return nil, &Error{Status: resp.StatusCode, Message: fmt.Sprintf("%d: %s", resp.StatusCode, msg)}
				}
			}
		}
		return nil, readError(resp.StatusCode, payload)
	}
	value, err := omap.FromJSON(payload)
	if err != nil {
		return nil, err
	}
	data, ok := value.(*omap.Map)
	if !ok {
		return nil, &Error{Status: resp.StatusCode, Message: "TypeSafe returned a non-object body"}
	}
	return data, nil
}

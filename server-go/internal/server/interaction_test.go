package server

import (
	"net/http/httptest"
	"testing"

	"github.com/satomic/model-router/server-go/internal/omap"
)

func keyRecord(id string) *omap.Map {
	key := omap.New()
	key.Set("id", id)
	return key
}

// The Copilot CLI over BYOK sends no interaction header, only x-initiator, and resends the same
// last user message on every turn of its tool-call loop. Those turns must share one derived id.
func TestDerivedInteractionGroupsACopilotCLILoop(t *testing.T) {
	prompt := "<current_datetime>2026-09-25T00:35:26.053+08:00</current_datetime>\n\nCreate fib.py"
	ids := map[any]bool{}
	for _, initiator := range []string{"user", "agent", "agent"} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("x-initiator", initiator)
		ids[derivedInteractionID(r, keyRecord("k1"), prompt)] = true
	}
	if len(ids) != 1 || ids[nil] {
		t.Fatalf("the turns of one loop should share one derived id, got %v", ids)
	}

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-initiator", "user")
	next := derivedInteractionID(r, keyRecord("k1"), "<current_datetime>2026-09-25T00:40:00</current_datetime>\n\nCreate fib.py")
	if ids[next] {
		t.Error("a new question (a new datetime stamp) must start a new interaction")
	}
	if ids[derivedInteractionID(r, keyRecord("k2"), prompt)] {
		t.Error("the same message on another API key must not share the interaction")
	}
}

// Without x-initiator the client never claimed to run a loop, so identical requests stay
// separate decisions; and a real interaction header always wins over derivation.
func TestDerivedInteractionNeedsTheInitiatorHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if id := derivedInteractionID(r, keyRecord("k1"), "What is 2+2?"); id != nil {
		t.Errorf("no x-initiator should derive nothing, got %v", id)
	}
	r.Header.Set("x-initiator", "user")
	if id := derivedInteractionID(r, keyRecord("k1"), "   "); id != nil {
		t.Errorf("an empty prompt should derive nothing, got %v", id)
	}
	r.Header.Set("x-interaction-id", "from-the-client")
	if got := interactionID(r); got != "from-the-client" {
		t.Errorf("the client's own header should be used as-is, got %v", got)
	}
}

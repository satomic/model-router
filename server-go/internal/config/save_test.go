package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satomic/model-router/server-go/internal/omap"
)

// fixture is shaped like a real config.yaml: a comment header, nested maps, block sequences,
// an empty string, a quoted numeric id, an empty list and a multi-line prompt. Every one of
// those is something a save has previously been able to mangle.
const fixture = `# Model Router configuration (live, gitignored).
#
# Holds api_key / client_secret / key_policy.github_token. Editable from the console's
# "Routing configuration" pages; a save writes this file back, so these comments survive.
strategy: ai
session:
  sticky: true
  ttl_seconds: 1800
auth:
  github:
    client_id: Ov23xxxx
    callback_url: ''
  admin_logins:
  - satomic
  key_policy:
    enabled: true
    enterprises:
      satomic:
        enabled: true
        teams:
        - '14501973'
providers:
  foundry:
    base_url: https://example.openai.azure.com/
    api_type: azure
models:
  gpt-4o:
    provider: foundry
    description: 快速、低成本的通用模型。
    default: true
  o3-pro:
    provider: foundry
    api: responses
rules:
- name: coding
  model: gpt-4o
  keywords:
  - code
  - 代码
ai_router:
  decision_model: gpt-4.1-mini
  decision_prompt: |
    You are a model router.

    Available models:
    {catalog}
model_groups:
  starter:
  - gpt-4o
  locked: []
`

// withFixture points the package at a temporary config.yaml holding the fixture, and restores
// the real paths afterwards.
func withFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	original := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = original })
	return path
}

// TestSaveRawPreservesAnUnchangedDocument is the property the console's round trip depends on:
// GET /v1/config, submit it back unedited, and the file on disk does not move by a single byte --
// not the comments, not the key order, not the quoting, not the sequence indentation, not a
// multi-line prompt's layout. Anything less means an unrelated save restyles a file the operator
// maintains by hand.
func TestSaveRawPreservesAnUnchangedDocument(t *testing.T) {
	path := withFixture(t)

	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveRaw(raw); err != nil {
		t.Fatal(err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != fixture {
		t.Errorf("an unedited round trip changed the file.\n--- want ---\n%s\n--- got ---\n%s", fixture, written)
	}
}

// TestSaveRawKeepsAMultiLinePromptReadable pins the one divergence that was not cosmetic: left
// to the emitter, the AI decision prompt came back as a single-quoted blob with every newline
// doubled. Valid YAML, and unusable by the operator who has to edit it.
func TestSaveRawKeepsAMultiLinePromptReadable(t *testing.T) {
	path := withFixture(t)
	raw, _ := LoadRaw()
	if _, err := SaveRaw(raw); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)
	text := string(written)

	if !strings.Contains(text, "decision_prompt: |") {
		t.Errorf("the prompt was not written as a literal block:\n%s", text)
	}
	if strings.Contains(text, "decision_prompt: '") {
		t.Errorf("the prompt came back folded and quoted:\n%s", text)
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Map("ai_router").Str("decision_prompt")
	want := "You are a model router.\n\nAvailable models:\n{catalog}\n"
	if got != want {
		t.Errorf("the prompt's value changed.\n  want %q\n  got  %q", want, got)
	}
}

// TestSaveRawIsIdempotent guards the worse failure: a save that keeps *growing* the file, which
// a naive comment round trip does by inserting a blank line on every pass.
func TestSaveRawIsIdempotent(t *testing.T) {
	path := withFixture(t)
	var previous string
	for pass := 1; pass <= 3; pass++ {
		raw, err := LoadRaw()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SaveRaw(raw); err != nil {
			t.Fatal(err)
		}
		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if pass > 1 && string(written) != previous {
			t.Fatalf("pass %d changed the file again; saves must converge", pass)
		}
		previous = string(written)
	}
}

// TestSaveRawReplacesOnlyTheSubmittedSections is the semantic the whole API depends on: a
// top-level key is replaced wholesale (so deleting a model works), and everything else is left
// exactly as it was, comments included.
func TestSaveRawReplacesOnlyTheSubmittedSections(t *testing.T) {
	path := withFixture(t)

	models := omap.New()
	only := omap.New()
	only.Set("provider", "foundry")
	models.Set("gpt-4o", only)
	updates := omap.New()
	updates.Set("models", models)

	cfg, err := SaveRaw(updates)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.Len() != 1 || !cfg.Models.Has("gpt-4o") {
		t.Errorf("models = %v, want only gpt-4o", cfg.Models.Keys())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	// The header comment and every untouched section must have survived.
	for _, want := range []string{
		"# Model Router configuration (live, gitignored).",
		"client_id: Ov23xxxx",
		"- '14501973'",
		"base_url: https://example.openai.azure.com/",
		"locked: []",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("an unrelated save lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "o3-pro") {
		t.Error("the replaced section kept a model the submission had removed")
	}
}

// TestSaveRawKeepsValueTypes: a team id is a quoted string and an empty string is empty. Both
// have changed type across a careless round trip before, and both break membership lookups.
func TestSaveRawKeepsValueTypes(t *testing.T) {
	path := withFixture(t)
	raw, _ := LoadRaw()
	if _, err := SaveRaw(raw); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)

	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	teams := reloaded.Map("auth").Map("key_policy").Map("enterprises").Map("satomic").Slice("teams")
	if len(teams) != 1 {
		t.Fatalf("teams = %v", teams)
	}
	if id, ok := teams[0].(string); !ok || id != "14501973" {
		t.Errorf("the team id came back as %#v, want the string \"14501973\"", teams[0])
	}
	if got := reloaded.Map("auth").Map("github").Value("callback_url"); got != "" {
		t.Errorf("callback_url = %#v, want an empty string", got)
	}
	if !strings.Contains(string(written), "{catalog}") {
		t.Error("the multi-line decision prompt did not survive")
	}
}

// TestSaveRawPreservesCRLF covers the file this project actually ships with on a Windows
// machine. The YAML parser keeps the \r inside every comment, and the emitter then reads it as a
// line break -- which turned a five-line header into one with a blank line after each line, and
// rewrote the whole file from CRLF to LF, on a save that changed nothing.
func TestSaveRawPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	crlf := strings.ReplaceAll(fixture, "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	original := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = original })

	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveRaw(raw); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)

	if !strings.Contains(text, "\r\n") {
		t.Error("a CRLF file was rewritten with Unix line endings")
	}
	if strings.Contains(text, "\r\n\r\n#") || strings.Contains(text, ".\r\n\r\nstrategy") {
		t.Errorf("a blank line appeared inside the comment header:\n%q", text[:200])
	}
	if got := strings.ReplaceAll(text, "\r\n", "\n"); got != fixture {
		t.Errorf("the CRLF round trip changed the document.\n--- want ---\n%s\n--- got ---\n%s", fixture, got)
	}
}

// TestSaveRawCRLFIsIdempotent is the failure that actually bites: each save added one blank line
// per comment line, so a file edited from the console a few times grew a header of blanks.
func TestSaveRawCRLFIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(fixture, "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	original := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = original })

	var previous string
	for pass := 1; pass <= 3; pass++ {
		raw, err := LoadRaw()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SaveRaw(raw); err != nil {
			t.Fatal(err)
		}
		written, _ := os.ReadFile(path)
		if pass > 1 && string(written) != previous {
			t.Fatalf("pass %d changed the file again; a CRLF save must converge", pass)
		}
		previous = string(written)
	}
}

// TestFlushSequencesRefusesToCorruptADocument is the safety net rather than the feature: the
// indentation rewrite works on rendered text, so it verifies itself by re-parsing, and anything
// it cannot reproduce exactly is left in the emitter's own style.
func TestFlushSequencesRefusesToCorruptADocument(t *testing.T) {
	// A dash at the start of a line *inside* a block scalar is content, not structure. Moving it
	// would change the prompt the operator wrote.
	const withDashInPrompt = `ai_router:
  decision_prompt: |
    Pick a model:
    - be brief
    - cite nothing
models:
  a:
    tags:
      - x
`
	got := flushSequences(withDashInPrompt)
	if !strings.Contains(got, "    - be brief") {
		t.Errorf("a dash inside a literal block was treated as structure:\n%s", got)
	}
	before, err := parseForCompare(withDashInPrompt)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseForCompare(got)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("the rewrite changed the document:\n  before %s\n  after  %s", before, after)
	}
}

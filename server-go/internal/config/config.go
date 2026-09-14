package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/localadmin"
	"github.com/satomic/model-router/server-go/internal/omap"
	"gopkg.in/yaml.v3"
)

const DefaultProviderName = "foundry"

// APITypes are the wire protocols a connection can speak. "azure" and "openai" are both the
// OpenAI chat-completions protocol and differ only in how the URL and the key are assembled;
// "anthropic" is the Anthropic Messages protocol, which is a different request and response
// shape altogether -- see internal/wire.
var APITypes = []string{"azure", "openai", "anthropic"}

// AnthropicVersion is sent as the `anthropic-version` header. A connection may override it
// through api_version, which for an Anthropic connection means that header rather than
// Azure's ?api-version=.
const AnthropicVersion = "2023-06-01"

// PolicyScopes are the scopes a model policy can bind a model group to, in the order the
// resolver reports them. The order carries no precedence: resolution is a union.
var PolicyScopes = []string{"user", "team", "organization"}

// Strategies are the routing strategies. "rule-then-ai" runs both: the rules decide when one
// of them matches, and only an unmatched request costs a decision call.
var Strategies = []string{"rule", "ai", "rule-then-ai"}

// CatalogPlaceholder stands for the model catalog inside the AI decision prompt.
// Rendering does a **literal replacement** rather than a format call: a custom prompt almost
// always contains JSON braces, which a formatter would treat as placeholders and then fail on.
const CatalogPlaceholder = "{catalog}"

const DefaultDecisionPrompt = `You are a model router. Given a user prompt, pick the single best backend model.

Available models:
{catalog}

Respond with ONLY a JSON object: {"model": "<model-name>", "rationale": "<one short sentence explaining why>"}.
The model name must be exactly one of the listed names.`

// Provider is one backend connection (Foundry / Azure OpenAI / any OpenAI-compatible
// service / an Anthropic Messages endpoint).
type Provider struct {
	Name       string
	BaseURL    string
	APIKey     string
	APIType    string
	APIVersion string
}

func newProvider(name string, raw *omap.Map) *Provider {
	p := &Provider{
		Name:    name,
		BaseURL: strings.TrimSpace(raw.Str("base_url")),
		APIKey:  strings.TrimSpace(raw.Str("api_key")),
		APIType: raw.Str("api_type"),
	}
	if p.APIType == "" {
		p.APIType = "azure"
	}
	p.APIVersion = raw.Str("api_version")
	if p.APIVersion == "" {
		// For an Anthropic connection this is the `anthropic-version` header, so it must not
		// inherit the Azure default: an Azure api-version string in that header is rejected
		// upstream.
		if p.APIType == "anthropic" {
			p.APIVersion = AnthropicVersion
		} else {
			p.APIVersion = EnvAPIVersion
		}
	}
	return p
}

// CacheKey identifies the connection for the client pool.
func (p *Provider) CacheKey() string {
	return strings.Join([]string{p.BaseURL, p.APIKey, p.APIType, p.APIVersion}, "\x00")
}

// Protocol is which wire protocol this connection speaks: "anthropic" or "openai".
//
// Everything above this line treats azure and openai as one protocol, because they are: the
// difference is confined to how the client is built. The protocol is what decides whether a
// request needs translating, so it is asked for by name rather than re-derived at each call site.
func (p *Provider) Protocol() string {
	if p.APIType == "anthropic" {
		return "anthropic"
	}
	return "openai"
}

// ResolvedModel is the routed model plus its provider and its real upstream model name.
type ResolvedModel struct {
	Name          string
	Meta          *omap.Map
	Provider      *Provider
	UpstreamModel string
	Reasoning     bool
	API           string
}

func newResolvedModel(name string, meta *omap.Map, provider *Provider) *ResolvedModel {
	r := &ResolvedModel{Name: name, Meta: meta, Provider: provider}
	// The upstream deployment name may differ from the name this router exposes
	// (e.g. openrouter's anthropic/claude-opus-5).
	r.UpstreamModel = meta.Str("model_name")
	if r.UpstreamModel == "" {
		r.UpstreamModel = name
	}
	r.Reasoning = meta.Bool("reasoning", false)
	r.API = "chat"
	if meta.Str("api") == "responses" {
		r.API = "responses"
	}
	return r
}

// RouterConfig is the parsed view of config.yaml that every request reads.
type RouterConfig struct {
	Raw *omap.Map

	// One scalar rather than two toggles, so a config can never describe a state the router
	// has no branch for.
	Strategy string
	Models   *omap.Map
	Rules    []any

	Sticky      bool
	SessionTTL  int
	MaxSessions int

	DecisionModel        string
	DecisionProviderName string
	DecisionTimeout      float64
	MaxPromptChars       int
	DecisionPrompt       string

	Providers           *omap.Map // name -> *Provider
	DefaultProviderName string

	// Named model groups. A group may legally be empty -- "this scope contributes nothing" is
	// a configuration an operator asks for on purpose.
	ModelGroups map[string][]string
	groupOrder  []string
	ModelPolicy *omap.Map

	Usage     *omap.Map
	AICredits *omap.Map

	GHClientID         string
	GHClientSecret     string
	GHCallbackURL      string
	AdminLogins        []string
	AllowAnyGitHubUser bool
	AuthSessionTTL     int
	KeyPolicy          *omap.Map
	KeyScopePolicy     *omap.Map
	LocalAdmin         *omap.Map
}

// New parses a raw configuration document.
func New(raw *omap.Map) *RouterConfig {
	if raw == nil {
		raw = omap.New()
	}
	c := &RouterConfig{Raw: raw}
	c.Strategy = raw.Str("strategy")
	if !raw.Has("strategy") {
		c.Strategy = "rule"
	}
	c.Models = raw.Map("models")
	if c.Models == nil {
		c.Models = omap.New()
	}
	c.Rules = raw.Slice("rules")

	session := raw.Map("session")
	c.Sticky = session.Bool("sticky", true)
	c.SessionTTL = session.Int("ttl_seconds", 1800)
	c.MaxSessions = session.Int("max_sessions", 10000)

	ai := raw.Map("ai_router")
	c.DecisionModel = ai.Str("decision_model")
	if !ai.Has("decision_model") {
		c.DecisionModel = "gpt-4.1"
	}
	c.DecisionProviderName = ai.Str("decision_provider")
	c.DecisionTimeout = ai.Float("timeout_seconds", 5)
	c.MaxPromptChars = ai.Int("max_prompt_chars", 4000)
	// Editable in the console. Empty or missing falls back to the built-in default.
	c.DecisionPrompt = strings.TrimSpace(ai.Str("decision_prompt"))
	if c.DecisionPrompt == "" {
		c.DecisionPrompt = DefaultDecisionPrompt
	}

	c.Providers = omap.New()
	if providers := raw.Map("providers"); providers != nil {
		for _, name := range providers.Keys() {
			meta := providers.Map(name)
			if meta == nil {
				meta = omap.New()
			}
			c.Providers.Set(name, newProvider(name, meta))
		}
	}
	if c.Providers.Len() == 0 && EnvEndpoint != "" {
		// Compatibility with old deployments that only have .env
		fallback := omap.New()
		fallback.Set("base_url", EnvEndpoint)
		fallback.Set("api_key", EnvAPIKey)
		fallback.Set("api_type", "azure")
		fallback.Set("api_version", EnvAPIVersion)
		c.Providers.Set(DefaultProviderName, newProvider(DefaultProviderName, fallback))
	}
	c.DefaultProviderName = raw.Str("default_provider")
	if c.DefaultProviderName == "" {
		if keys := c.Providers.Keys(); len(keys) > 0 {
			c.DefaultProviderName = keys[0]
		} else {
			c.DefaultProviderName = DefaultProviderName
		}
	}

	c.ModelGroups = map[string][]string{}
	if groups := raw.Map("model_groups"); groups != nil {
		for _, name := range groups.Keys() {
			c.groupOrder = append(c.groupOrder, name)
			c.ModelGroups[name] = omap.StringSlice(groups.Value(name))
			if c.ModelGroups[name] == nil {
				c.ModelGroups[name] = []string{}
			}
		}
	}
	// Defaults to an empty object, i.e. disabled, so upgrading an existing deployment does
	// not suddenly restrict anybody.
	c.ModelPolicy = orEmpty(raw.Map("model_policy"))
	c.Usage = orEmpty(raw.Map("usage"))
	// Absent means off on both counts, so upgrading changes nothing.
	c.AICredits = orEmpty(raw.Map("ai_credits"))

	auth := orEmpty(raw.Map("auth"))
	gh := orEmpty(auth.Map("github"))
	c.GHClientID = strings.TrimSpace(gh.Str("client_id"))
	c.GHClientSecret = strings.TrimSpace(gh.Str("client_secret"))
	c.GHCallbackURL = strings.TrimSpace(gh.Str("callback_url"))
	for _, login := range omap.StringSlice(auth.Value("admin_logins")) {
		if trimmed := strings.TrimSpace(login); trimmed != "" {
			c.AdminLogins = append(c.AdminLogins, strings.ToLower(trimmed))
		}
	}
	c.AllowAnyGitHubUser = auth.Bool("allow_any_github_user", true)
	c.AuthSessionTTL = auth.Int("session_ttl_seconds", 7*24*3600)
	c.KeyPolicy = orEmpty(auth.Map("key_policy"))
	// Absent means disabled, and disabled means nobody -- the closed default, unlike
	// key_policy above.
	c.KeyScopePolicy = orEmpty(auth.Map("key_scope_policy"))
	// Enabled by default: without it a deployment that cannot reach github.com has no way in.
	c.LocalAdmin = orEmpty(auth.Map("local_admin"))
	return c
}

func orEmpty(m *omap.Map) *omap.Map {
	if m == nil {
		return omap.New()
	}
	return m
}

func (c *RouterConfig) OAuthConfigured() bool {
	return c.GHClientID != "" && c.GHClientSecret != ""
}

// UsageRollupSeconds is how often the Usage statistics are recomputed in the background.
func (c *RouterConfig) UsageRollupSeconds(defaultInterval float64) float64 {
	value := c.Usage.Float("rollup_interval_seconds", 0)
	if value <= 0 {
		return defaultInterval
	}
	if value < 60 {
		return 60
	}
	return value
}

// UsageRollupDays is how far back the rollup reaches. It must cover the longest range the
// console offers, or that button would show a window the data does not contain.
func (c *RouterConfig) UsageRollupDays(defaultDays int) int {
	value := c.Usage.Int("rollup_days", 0)
	if value <= 0 {
		return defaultDays
	}
	if value < 1 {
		return 1
	}
	return value
}

func (c *RouterConfig) LocalAdminEnabled() bool {
	return c.LocalAdmin.Bool("enabled", true)
}

func (c *RouterConfig) LocalAdminUsername() string {
	name := strings.TrimSpace(c.LocalAdmin.Str("username"))
	if name == "" {
		return localadmin.DefaultUsername
	}
	return name
}

// LocalAdminSettings is the view internal/localadmin needs, so that package does not
// have to import this one.
func (c *RouterConfig) LocalAdminSettings() localadmin.Settings {
	return localadmin.Settings{
		Enabled:      c.LocalAdminEnabled(),
		Username:     c.LocalAdminUsername(),
		PasswordHash: c.LocalAdmin.Str("password_hash"),
		PasswordSalt: c.LocalAdmin.Str("password_salt"),
	}
}

// IsLocalAdminLogin reports whether `login` is *the* local administrator right now.
//
// Recomputed per request like IsAdminLogin, so disabling or renaming the account in
// config.yaml downgrades sessions that were already issued to it.
func (c *RouterConfig) IsLocalAdminLogin(login string) bool {
	if !c.LocalAdminEnabled() {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(login), c.LocalAdminUsername())
}

func (c *RouterConfig) GHAdminToken() string {
	return strings.TrimSpace(c.KeyPolicy.Str("github_token"))
}

// KeyScopePolicyEnabled reports whether anybody at all may narrow an API key's scope.
//
// Defaults to false, and false means "nobody" rather than "everybody": a key with no scope
// already reaches everything its owner may reach, so the closed default takes no capability
// away from an existing deployment.
func (c *RouterConfig) KeyScopePolicyEnabled() bool {
	return c.KeyScopePolicy.Bool("enabled", false)
}

// ModelPolicyEnabled reports whether the model policy is enforced at all.
//
// Defaults to false so that adding the section to config.yaml without turning it on changes
// nothing: an operator can build up groups and bindings first, then enable.
func (c *RouterConfig) ModelPolicyEnabled() bool {
	return c.ModelPolicy.Bool("enabled", false)
}

// DefaultGroup is the group every signed-in user starts with, before any scope binding
// applies. An empty string means "no default".
func (c *RouterConfig) DefaultGroup() string {
	return strings.TrimSpace(c.ModelPolicy.Str("default_group"))
}

func (c *RouterConfig) HasModelGroup(name string) bool {
	_, ok := c.ModelGroups[name]
	return ok
}

func (c *RouterConfig) ModelGroupNames() []string {
	out := make([]string, len(c.groupOrder))
	copy(out, c.groupOrder)
	return out
}

// RestrictedTo returns a view of this configuration whose model catalog is narrowed to `names`.
//
// This is how the model policy is enforced, and it is deliberately a *narrowing of the
// catalog* rather than a check bolted onto each routing strategy. Everything downstream
// already treats "not in the models catalog" as a first-class case: MatchRules skips such a
// rule with a recorded reason, RouteByAI only ever offers the catalog as candidates and falls
// back when the answer is not one of them, and DefaultModel reads from the same map.
// Narrowing therefore makes a disallowed model unreachable through every path at once.
//
// The view shares everything else by reference: it is read-only and per-request, so copying
// the provider objects would only cost the connection pool its cache keys. Order follows the
// full catalog, so a narrowed decision prompt lists models in the same order the Models page does.
func (c *RouterConfig) RestrictedTo(names []string) *RouterConfig {
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	view := *c
	narrowed := omap.New()
	for _, name := range c.Models.Keys() {
		if allowed[name] {
			narrowed.Set(name, c.Models.Value(name))
		}
	}
	view.Models = narrowed
	return &view
}

// GroupModels are the models in `group`, filtered to what the catalog still has.
//
// Filtering here rather than at save time keeps a group honest after a model is deleted
// straight out of config.yaml by hand: a group naming a model that no longer exists must not
// make that name routable.
func (c *RouterConfig) GroupModels(group string) []string {
	out := []string{}
	for _, name := range c.ModelGroups[group] {
		if c.Models.Has(name) {
			out = append(out, name)
		}
	}
	return out
}

func (c *RouterConfig) IsAdminLogin(login string) bool {
	lowered := strings.ToLower(login)
	for _, admin := range c.AdminLogins {
		if admin == lowered {
			return true
		}
	}
	return false
}

func (c *RouterConfig) DefaultModel() string {
	for _, name := range c.Models.Keys() {
		if c.Models.Map(name).Bool("default", false) {
			return name
		}
	}
	if keys := c.Models.Keys(); len(keys) > 0 {
		return keys[0]
	}
	return ""
}

// GetProvider looks a provider up by name, falling back to the default when missing.
func (c *RouterConfig) GetProvider(name string) *Provider {
	if name != "" {
		if p, ok := c.Providers.Get(name); ok {
			return p.(*Provider)
		}
	}
	if p, ok := c.Providers.Get(c.DefaultProviderName); ok {
		return p.(*Provider)
	}
	if keys := c.Providers.Keys(); len(keys) > 0 {
		return c.Providers.Value(keys[0]).(*Provider)
	}
	// Nothing configured at all: return an empty provider so the eventual call fails with a
	// clearer message than a lookup panic.
	return newProvider(DefaultProviderName, omap.New())
}

func (c *RouterConfig) ModelMeta(name string) *omap.Map {
	return orEmpty(c.Models.Map(name))
}

func (c *RouterConfig) ResolveModel(name string) *ResolvedModel {
	meta := c.ModelMeta(name)
	return newResolvedModel(name, meta, c.GetProvider(meta.Str("provider")))
}

// ModelCatalogText is the catalog fed to the decision model: one `- name: description`
// per line.
func (c *RouterConfig) ModelCatalogText() string {
	lines := make([]string, 0, c.Models.Len())
	for _, name := range c.Models.Keys() {
		lines = append(lines, fmt.Sprintf("- %s: %s", name, strings.TrimSpace(c.ModelMeta(name).Str("description"))))
	}
	return strings.Join(lines, "\n")
}

// RenderDecisionPrompt fills the model catalog into the prompt template, yielding the exact
// system content sent to the decision model.
//
// Literal replacement rather than a formatter: user-written prompts usually contain JSON
// braces. When the placeholder is missing the catalog is appended so the decision model can
// at least see the candidates.
func (c *RouterConfig) RenderDecisionPrompt(catalog string) string {
	if strings.Contains(c.DecisionPrompt, CatalogPlaceholder) {
		return strings.ReplaceAll(c.DecisionPrompt, CatalogPlaceholder, catalog)
	}
	return c.DecisionPrompt + "\n\nAvailable models:\n" + catalog
}

// ResolveDecisionModel prefers the decision model's metadata from `models`, otherwise treats
// it as a bare deployment name on the provider.
func (c *RouterConfig) ResolveDecisionModel() *ResolvedModel {
	meta := c.ModelMeta(c.DecisionModel)
	providerName := c.DecisionProviderName
	if providerName == "" {
		providerName = meta.Str("provider")
	}
	return newResolvedModel(c.DecisionModel, meta, c.GetProvider(providerName))
}

// -- Reading and writing config.yaml ------------------------------------------

// saveMu serialises the read-modify-write of config.yaml. Concurrent console saves would
// otherwise interleave and lose one another's sections.
var saveMu sync.Mutex

// LoadRaw reads config.yaml, seeding it from the template when it does not exist yet.
//
// Seeding here rather than at startup covers every entry point instead of only whichever one
// remembered to call it.
func LoadRaw() (*omap.Map, error) {
	if _, err := EnsureConfigFile(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(toLF(data), &doc); err != nil {
		return nil, err
	}
	value, err := omap.FromYAMLNode(&doc)
	if err != nil {
		return nil, err
	}
	if m, ok := value.(*omap.Map); ok {
		return m, nil
	}
	return omap.New(), nil
}

func Load() (*RouterConfig, error) {
	raw, err := LoadRaw()
	if err != nil {
		return nil, err
	}
	return New(raw), nil
}

// SaveRaw merges the submitted configuration back into config.yaml. Top-level keys are
// **replaced wholesale** and file comments outside the replaced sections are preserved.
//
// Merging happens at the top level only: {"auth": {...}} replaces the entire auth section
// instead of merging field by field. That is deliberate -- deleting entries from `models` /
// `providers` / `rules` depends on this semantic. Switch it to a recursive merge and models
// the user deleted would come back on the next save.
//
// Callers must therefore submit the **complete** top-level section: to change only
// auth.key_policy, read the existing auth first and write the whole section back, or the
// github credentials and admin_logins in that same section get wiped.
func SaveRaw(updates *omap.Map) (*RouterConfig, error) {
	saveMu.Lock()
	defer saveMu.Unlock()

	if _, err := EnsureConfigFile(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, err
	}
	crlf := bytes.Contains(data, []byte("\r\n"))
	var doc yaml.Node
	if err := yaml.Unmarshal(toLF(data), &doc); err != nil {
		return nil, err
	}
	root := documentMapping(&doc)

	for _, key := range updates.Keys() {
		setMappingKey(root, key, omap.ToYAMLNode(updates.Value(key)))
	}

	var buf strings.Builder
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	rendered := flushSequences(buf.String())
	if crlf {
		// The file was written with Windows line endings, most likely by the editor the
		// operator uses. Converting it to LF on an unrelated save would show up as a
		// whole-file change in every diff they look at afterwards.
		rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	}
	tmp := ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(rendered), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, ConfigPath); err != nil {
		return nil, err
	}

	value, err := omap.FromYAMLNode(&doc)
	if err != nil {
		return nil, err
	}
	merged, _ := value.(*omap.Map)
	return New(orEmpty(merged)), nil
}

// flushSequences rewrites the emitter's indented block sequences into the flush style, where a
// sequence's dashes line up with the key that introduces them:
//
//	admin_logins:        instead of    admin_logins:
//	- satomic                            - satomic
//
// Both parse identically and neither is more correct, but the Python backend's writer produces
// the flush form, and a file that flips between the two on every save is 80 lines of noise in
// any diff an operator looks at. go-yaml's encoder has no option for it, so it is done on the
// rendered text -- and then checked, because this is a file full of credentials and a
// mis-indented line is a silently different document.
//
// The check is what makes the rewrite safe rather than clever: the result is re-parsed and
// compared against the text it came from, and on any mismatch at all the emitter's own output is
// returned untouched. The worst case is therefore the indented style, never a damaged file.
func flushSequences(text string) string {
	rewritten := dedentSequenceBlocks(text)
	if rewritten == text {
		return text
	}
	before, err := parseForCompare(text)
	if err != nil {
		return text
	}
	after, err := parseForCompare(rewritten)
	if err != nil || before != after {
		return text
	}
	return rewritten
}

// parseForCompare renders a document to a canonical string, so two spellings of the same
// document compare equal and any real difference does not.
func parseForCompare(text string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	value, err := omap.FromYAMLNode(&doc)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// dedentSequenceBlocks removes the one indentation level the emitter puts in front of every
// block sequence.
//
// Each enclosing sequence costs two spaces, hence the stack: a dash nested inside another
// sequence item shifts by four. Block scalars are stepped over untouched -- their body is
// content, and a dash inside a decision prompt is not structure.
func dedentSequenceBlocks(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var blocks []int // the emitted indents of the sequence blocks currently open
	blockScalar := -1

	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)

		if blockScalar >= 0 {
			// Inside a literal or folded scalar until a line appears at or left of its key.
			if trimmed == "" || indent > blockScalar {
				out = append(out, line)
				continue
			}
			blockScalar = -1
		}
		if trimmed == "" {
			out = append(out, line)
			continue
		}

		for len(blocks) > 0 && indent < blocks[len(blocks)-1] {
			blocks = blocks[:len(blocks)-1]
		}
		isDash := trimmed == "-" || strings.HasPrefix(trimmed, "- ")
		if isDash && (len(blocks) == 0 || indent > blocks[len(blocks)-1]) {
			blocks = append(blocks, indent)
		}
		if shift := 2 * len(blocks); shift > 0 && indent >= shift {
			line = line[shift:]
		}
		out = append(out, line)

		// A key whose value is a block scalar ends in the indicator, possibly with a chomping
		// or explicit-indent modifier.
		if body := strings.TrimRight(trimmed, "-+0123456789"); strings.HasSuffix(body, "|") || strings.HasSuffix(body, ">") {
			blockScalar = indent
		}
	}
	return strings.Join(out, "\n")
}

// toLF normalises Windows line endings before the document is parsed.
//
// This has to happen on the bytes, not on the node tree afterwards: given CRLF input the YAML
// parser reads each \r as a blank line of its own and stores the header comment already doubled
// -- "# a\n\n# b" where LF input gives "# a\n# b" -- and it attaches the comment to the document
// node rather than to the first key. By the time a caller could inspect the tree the \r is gone
// and the damage is indistinguishable from a comment the operator wrote with blank lines in it.
//
// Left alone it grew the file on every save: one extra blank line per comment line, each time.
// SaveRaw puts the CRLF back when it writes, so the file keeps the convention it arrived with.
func toLF(data []byte) []byte {
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

// documentMapping returns the document's root mapping, creating one when the file was empty.
func documentMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind != yaml.DocumentNode {
		*doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		return doc.Content[0]
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		*root = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	return root
}

// setMappingKey replaces one key's value in place, keeping the key node -- and therefore the
// comments attached to it -- exactly where it was. A new key is appended at the end.
func setMappingKey(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			// Carry the old value's comments across: they document the section, and the
			// console's round-trip must not strip them.
			old := mapping.Content[i+1]
			value.HeadComment = old.HeadComment
			value.LineComment = old.LineComment
			value.FootComment = old.FootComment
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

// number is a small helper for validators that need to know a value is numeric and not a bool.
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	default:
		return 0, false
	}
}

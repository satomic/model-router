// Package routing holds the routing strategies: rule (keyword/length rules), ai (decision
// model), and rule-then-ai (rules first, the decision model only when no rule matched). All
// emit the full decision analysis.
package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/satomic/model-router/server-go/internal/config"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/upstream"
)

// Clients such as Copilot wrap the real question in a <userRequest> tag, preceded by a large
// block of context.
var userRequestRE = regexp.MustCompile(`(?s)<userRequest>\s*(.*?)\s*</userRequest>`)

// ExtractUserPrompt takes the latest user message as the routing input; when it contains a
// <userRequest> tag, only the real question is kept.
func ExtractUserPrompt(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(*omap.Map)
		if !ok || msg.Str("role") != "user" {
			continue
		}
		content := msg.Value("content")
		var text string
		if parts, ok := content.([]any); ok { // multimodal content parts
			pieces := []string{}
			for _, part := range parts {
				if block, ok := part.(*omap.Map); ok {
					pieces = append(pieces, block.Str("text"))
				}
			}
			text = strings.Join(pieces, " ")
		} else {
			text = omap.AsString(content)
		}
		if matches := userRequestRE.FindAllStringSubmatch(text, -1); len(matches) > 0 {
			return matches[len(matches)-1][1]
		}
		return text
	}
	return ""
}

// TruncateForDecision keeps half of the head and half of the tail of an over-long prompt, so
// the real question at the end is not cut off.
func TruncateForDecision(prompt string, maxChars int) string {
	runes := []rune(prompt)
	if len(runes) <= maxChars {
		return prompt
	}
	half := maxChars / 2
	return string(runes[:half]) + "\n...[...omitted...]...\n" + string(runes[len(runes)-half:])
}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// MatchRules evaluates the rules in order, returning (model, rule name, evaluated steps) for
// the first hit, or ("", "", evaluated) when nothing matched.
//
// Separate from RouteByRules because "no rule matched" is a decision in its own right under
// the rule-then-ai strategy: there it hands the request to the decision model rather than to
// the default model. Returning an empty model instead of substituting a default keeps that
// distinction, and keeps the substitution in exactly one place per strategy.
func MatchRules(prompt string, cfg *config.RouterConfig) (string, string, []any) {
	evaluated := []any{}
	promptLen := len([]rune(prompt))
	for _, item := range cfg.Rules {
		rule, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		name := rule.Str("name")
		if !rule.Has("name") {
			name = "unnamed"
		}
		model := rule.Str("model")
		step := mapOf("rule", name, "model", rule.Value("model"), "matched", false)
		if !cfg.Models.Has(model) {
			step.Set("skipped", fmt.Sprintf("model '%s' is not in the models catalog", model))
			evaluated = append(evaluated, step)
			continue
		}
		if minChars := rule.Int("min_prompt_chars", 0); minChars > 0 {
			step.Set("check", fmt.Sprintf("len(prompt)=%d >= %d", promptLen, minChars))
			if promptLen >= minChars {
				step.Set("matched", true)
				evaluated = append(evaluated, step)
				return model, name, evaluated
			}
		}
		keywords := omap.StringSlice(rule.Value("keywords"))
		if len(keywords) > 0 {
			quoted := make([]string, len(keywords))
			for i, keyword := range keywords {
				quoted[i] = regexp.QuoteMeta(keyword)
			}
			pattern, err := regexp.Compile("(?i)" + strings.Join(quoted, "|"))
			step.Set("check", "keywords="+pythonList(keywords))
			if err == nil {
				if found := pattern.FindString(prompt); found != "" {
					step.Set("matched", true)
					step.Set("matched_keyword", found)
					evaluated = append(evaluated, step)
					return model, name, evaluated
				}
			}
		}
		evaluated = append(evaluated, step)
	}
	return "", "", evaluated
}

// pythonList renders a keyword list the way the Python backend's repr() did, so a trace
// written by either backend reads identically in the console.
func pythonList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "'" + strings.ReplaceAll(item, "'", "\\'") + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// RouteByRules returns (model, reason, analysis). The analysis records how each rule was
// evaluated.
func RouteByRules(prompt string, cfg *config.RouterConfig) (string, string, *omap.Map) {
	model, name, evaluated := MatchRules(prompt, cfg)
	if model != "" {
		return model, name, mapOf("type", "rule", "evaluated", evaluated)
	}
	return cfg.DefaultModel(), "default", mapOf(
		"type", "rule",
		"evaluated", evaluated,
		"fallback", "no rule matched, using the default model",
	)
}

// RouteCombined runs the rules first, and the AI decision model only when no rule matched.
//
// Both strategies are configured and both are live. The rules win when one of them fires,
// which is the point of the strategy: a keyword rule is the operator stating an explicit
// intent, and an explicit intent should not be second-guessed by a classifier -- nor should it
// be paid for with a decision call it cannot change.
//
// A rule that matched on min_prompt_chars rather than on a keyword also wins. It is just as
// explicitly configured, and making the Rules page authoritative for some of its own rows and
// advisory for others would be impossible to reason about from the UI.
//
// The analysis nests both sub-analyses under their own keys, each keeping the `type` its
// single-strategy counterpart emits, so the console renders the handover with the renderers it
// already has instead of a third copy of them.
func RouteCombined(ctx context.Context, prompt string, cfg *config.RouterConfig, pool *upstream.Pool) (string, string, *omap.Map) {
	model, name, evaluated := MatchRules(prompt, cfg)
	ruleAnalysis := mapOf("type", "rule", "evaluated", evaluated)
	if model != "" {
		return model, name, mapOf(
			"type", "rule-then-ai",
			"decided_by", "rule",
			"rule", ruleAnalysis,
		)
	}
	// No rule fired, so the decision model gets the request. Its own fallback to the default
	// model stays inside RouteByAI -- from here it is one strategy that answers or does not.
	ruleAnalysis.Set("fallback", "no rule matched, handing over to the AI decision model")
	aiModel, aiReason, aiAnalysis := RouteByAI(ctx, prompt, cfg, pool)
	return aiModel, aiReason, mapOf(
		"type", "rule-then-ai",
		"decided_by", "ai",
		"rule", ruleAnalysis,
		"ai", aiAnalysis,
	)
}

// RouteByAI returns (model, reason, analysis). The analysis records the decision model's
// input, output and rationale.
//
// Three engines make the decision (ai_router.decision_engine): an LLM reached through the
// connection pool -- so it can use a different endpoint/key than the serving models --
// TypeSafe's hosted Jev, or a self-hosted Laya on the same protocol. All share the analysis skeleton, the fallback to the default model, and the
// rule that an answer outside the catalog is a failure, so the strategy above them cannot tell
// which one ran.
func RouteByAI(ctx context.Context, prompt string, cfg *config.RouterConfig, pool *upstream.Pool) (string, string, *omap.Map) {
	truncated := TruncateForDecision(prompt, cfg.MaxPromptChars)

	candidates := make([]any, 0, cfg.Models.Len())
	for _, name := range cfg.Models.Keys() {
		candidates = append(candidates, name)
	}
	analysis := mapOf(
		"type", "ai",
		"decision_engine", cfg.DecisionEngine,
		"decision_input", headRunes(truncated, 500),
		"prompt_truncated", len([]rune(prompt)) > cfg.MaxPromptChars,
		"candidates", candidates,
	)

	start := time.Now()
	fail := func(message string) (string, string, *omap.Map) {
		analysis.Set("decision_latency_ms", round1(float64(time.Since(start).Microseconds())/1000))
		analysis.Set("error", message)
		log.Printf("WARNING mr: AI decision failed (%s), falling back to the default model", message)
		analysis.Set("fallback", true)
		return cfg.DefaultModel(), "ai-fallback-default", analysis
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.DecisionTimeout*float64(time.Second)))
	defer cancel()

	var choice string
	if endpoint, ok := cfg.SystemOne(); ok {
		picked, err := decideBySystemOne(callCtx, truncated, cfg, endpoint, analysis)
		if err != nil {
			return fail(err.Error())
		}
		choice = picked
	} else {
		picked, err := decideByLLM(callCtx, truncated, cfg, pool, analysis)
		if err != nil {
			return fail(err.Error())
		}
		choice = picked
	}
	analysis.Set("decision_latency_ms", round1(float64(time.Since(start).Microseconds())/1000))

	if cfg.Models.Has(choice) {
		return choice, "ai-decision", analysis
	}
	analysis.Set("error", fmt.Sprintf("the decision model returned unknown model '%s'", choice))
	log.Printf("WARNING mr: AI decision returned unknown model '%s', falling back to default", choice)
	analysis.Set("fallback", true)
	return cfg.DefaultModel(), "ai-fallback-default", analysis
}

// decideByLLM asks a chat model for {"model", "rationale"} JSON, with the decision prompt as
// the system message.
func decideByLLM(ctx context.Context, truncated string, cfg *config.RouterConfig, pool *upstream.Pool,
	analysis *omap.Map) (string, error) {
	systemPrompt := cfg.RenderDecisionPrompt(cfg.ModelCatalogText())
	decision := cfg.ResolveDecisionModel()
	analysis.Set("decision_model", cfg.DecisionModel)
	analysis.Set("decision_provider", decision.Provider.Name)
	// The prompt is configurable, so the trace must keep the system content that was actually
	// sent; otherwise there is no way to tell afterwards which version a historical request used.
	analysis.Set("decision_system", systemPrompt)

	client, err := pool.Get(decision.Provider, "chat")
	if err != nil {
		return "", err
	}
	body := mapOf(
		"model", decision.UpstreamModel,
		"messages", []any{
			mapOf("role", "system", "content", systemPrompt),
			mapOf("role", "user", "content", truncated),
		},
		"max_tokens", json.Number("120"),
		"temperature", json.Number("0"),
		"response_format", mapOf("type", "json_object"),
	)
	resp, err := client.Create(ctx, decision.UpstreamModel, body)
	if err != nil {
		return "", err
	}

	raw := ""
	if choices := resp.Slice("choices"); len(choices) > 0 {
		if choice, ok := choices[0].(*omap.Map); ok {
			if message := choice.Map("message"); message != nil {
				raw = message.Str("content")
			}
		}
	}
	analysis.Set("raw_response", raw)
	if usage := resp.Map("usage"); usage != nil {
		analysis.Set("decision_usage", mapOf(
			"prompt_tokens", usage.Value("prompt_tokens"),
			"completion_tokens", usage.Value("completion_tokens"),
		))
	}

	value, err := omap.FromJSON([]byte(raw))
	if err != nil {
		return "", err
	}
	data, ok := value.(*omap.Map)
	if !ok {
		return "", fmt.Errorf("the decision model did not return a JSON object")
	}
	analysis.Set("rationale", data.Value("rationale"))
	return data.Str("model"), nil
}

// SystemOneInstructions is the Choice question Jev (or Laya) answers. Jev reads its
// instructions literally, so this says exactly what to weigh rather than leaving "best" to
// interpretation; the model descriptions carry the rest, one per option.
const SystemOneInstructions = "Which backend model should handle `user_request`? " +
	"Pick the option whose description best matches what the request needs. " +
	"Prefer a cheaper, faster model when the request is simple, and a stronger model only when the request needs it."

// SystemOneQuestionID is the key the routing question is asked (and answered) under.
const SystemOneQuestionID = "model"

// BuildSystemOneRequest is the /v1/systemone body for one routing decision: the user's request
// as the state, and a single Choice whose options are the model catalog with each model's
// description as its rubric. Jev and Laya get the same body, bar the model field -- Laya is
// asked without one when no checkpoint is pinned, so it picks by the request's language.
// Exported so the console's preview shows the exact request.
func BuildSystemOneRequest(truncated string, cfg *config.RouterConfig, endpoint config.SystemOneEndpoint) *omap.Map {
	criteria := omap.New()
	for _, name := range cfg.Models.Keys() {
		// A model without a description is still a valid option; null is how the API spells
		// "no rubric", rather than an empty string that reads as a description.
		if desc := strings.TrimSpace(cfg.ModelMeta(name).Str("description")); desc != "" {
			criteria.Set(name, desc)
		} else {
			criteria.Set(name, nil)
		}
	}
	body := omap.New()
	if endpoint.Model != "" {
		body.Set("model", endpoint.Model)
	}
	body.Set("state", mapOf("user_request", truncated))
	body.Set("questions", mapOf(
		SystemOneQuestionID, mapOf(
			"type", "choice",
			"instructions", SystemOneInstructions,
			"criteria", criteria,
		),
	))
	return body
}

// decideBySystemOne asks Jev or Laya one Choice over the catalog. There is no free-text
// rationale to record, so the probabilities and confidence stand in for it -- they are the
// more useful record anyway: they show how close the runner-up was.
func decideBySystemOne(ctx context.Context, truncated string, cfg *config.RouterConfig,
	endpoint config.SystemOneEndpoint, analysis *omap.Map) (string, error) {
	model := endpoint.Model
	if model == "" {
		model = "auto"
	}
	analysis.Set("decision_model", model)
	analysis.Set("decision_provider", endpoint.Engine)
	if endpoint.KeyNeeded && endpoint.APIKey == "" {
		return "", fmt.Errorf("no TypeSafe API key: set ai_router.typesafe.api_key or TYPESAFE_API_KEY")
	}
	if endpoint.BaseURL == "" {
		return "", fmt.Errorf("no %s address: set ai_router.%s.base_url", endpoint.Label, endpoint.Engine)
	}
	if n := cfg.Models.Len(); n < 2 || n > endpoint.MaxOptions {
		// One option is not a decision, and past the service's cap the request is refused.
		// Either way the default model is the honest answer, and saying why beats a 4xx.
		return "", fmt.Errorf("%s needs between 2 and %d candidate models, got %d", endpoint.Label, endpoint.MaxOptions, n)
	}
	body := BuildSystemOneRequest(truncated, cfg, endpoint)
	question := body.Map("questions").Map(SystemOneQuestionID)
	analysis.Set("decision_question", mapOf(
		"instructions", question.Value("instructions"),
		"criteria", question.Value("criteria"),
	))

	resp, err := upstream.SystemOne(ctx, endpoint.BaseURL, endpoint.APIKey, body)
	if err != nil {
		return "", err
	}
	if version := resp.Str("model"); version != "" {
		analysis.Set("decision_model_version", version)
	}
	// Laya reports which of its checkpoints answered and why (the request's language).
	if routed := resp.Map("routing"); routed != nil {
		analysis.Set("decision_checkpoint", mapOf("model", routed.Value("model"), "reason", routed.Value("reason")))
	}
	if usage := resp.Map("usage"); usage != nil {
		analysis.Set("decision_usage", mapOf(
			"prompt_tokens", usage.Value("input_tokens"),
			"completion_tokens", usage.Value("output_tokens"),
		))
	}
	answer := resp.Map("answers").Map(SystemOneQuestionID)
	if answer == nil {
		return "", fmt.Errorf("%s returned no answer for the routing question", endpoint.Label)
	}
	if raw, err := json.Marshal(answer); err == nil {
		analysis.Set("raw_response", string(raw))
	}
	choice := answer.Str("choice")
	analysis.Set("probabilities", answer.Value("probabilities"))
	analysis.Set("confidence", answer.Value("confidence"))
	name := "Jev"
	if endpoint.Engine == "laya" {
		name = "Laya"
	}
	analysis.Set("rationale", fmt.Sprintf("%s picked %s with p=%s (confidence %s)",
		name, choice, numberText(answer.Map("probabilities").Value(choice)), numberText(answer.Value("confidence"))))
	return choice, nil
}

func numberText(v any) string {
	switch n := v.(type) {
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return fmt.Sprintf("%.2f", f)
		}
		return n.String()
	case float64:
		return fmt.Sprintf("%.2f", n)
	case nil:
		return "?"
	}
	return fmt.Sprint(v)
}

func headRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func round1(v float64) json.Number {
	return json.Number(fmt.Sprintf("%.1f", v))
}

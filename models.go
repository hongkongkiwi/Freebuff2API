package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	upstreamConstantsPath = "common/src/constants/"
	upstreamAgentsFile    = "free-agents.ts"
	upstreamModelsFile    = "freebuff-models.ts"
	upstreamModelIDsFile  = "freebuff-model-ids.ts"
	upstreamModelCfgFile  = "model-config.ts"
	rawGitHubBase         = "https://raw.githubusercontent.com/CodebuffAI/freebuff/main/"
	jsDelivrBase          = "https://cdn.jsdelivr.net/gh/CodebuffAI/freebuff@main/"
	modelRefreshInterval  = 6 * time.Hour
	defaultServingModelID = "deepseek/deepseek-v4-flash"
)

// hardcodedFallback is used when every upstream fetch fails on startup.
// Snapshot of the freebuff catalog at implementation time (2026-09-30).
var hardcodedFallback = map[string][]string{
	"base2-free":                      {"minimax/minimax-m3", "openai/gpt-5.6-luna", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash"},
	"base2-free-claude-opus-4-8":      {"anthropic/claude-opus-4.8"},
	"base2-free-claude-opus-5":        {"anthropic/claude-opus-5"},
	"base2-free-claude-opus-5-5":      {"anthropic/claude-opus-5.5"},
	"base2-free-claude-sonnet-4-6":    {"anthropic/claude-sonnet-4.6"},
	"base2-free-claude-sonnet-5":      {"anthropic/claude-sonnet-5"},
	"base2-free-codestral-2508":       {"mistralai/codestral-2508"},
	"base2-free-deepseek":             {"deepseek/deepseek-v4-pro"},
	"base2-free-deepseek-flash":       {"deepseek/deepseek-v4-flash"},
	"base2-free-deepseek-v4-1-flash":  {"deepseek/deepseek-v4.1-flash"},
	"base2-free-fable":                {"anthropic/claude-fable-5.1"},
	"base2-free-gemini-3-5-flash":     {"google/gemini-3.5-flash"},
	"base2-free-gemini-3-6-flash":     {"google/gemini-3.6-flash"},
	"base2-free-gemini-3-7-flash":     {"google/gemini-3.7-flash"},
	"base2-free-gemini-3-8-flash":     {"google/gemini-3.8-flash"},
	"base2-free-glm":                  {"z-ai/glm-5.2"},
	"base2-free-glm-5-3":              {"z-ai/glm-5.3"},
	"base2-free-glm-5-3-flash":        {"z-ai/glm-5.3-flash"},
	"base2-free-glm-5-3-flashx":       {"z-ai/glm-5.3-flashx"},
	"base2-free-glm-5-3-prime":        {"z-ai/glm-5.3-prime"},
	"base2-free-glm-5-turbo":          {"z-ai/glm-5-turbo"},
	"base2-free-gpt-5-4-pro":          {"openai/gpt-5.4-pro"},
	"base2-free-gpt-5-5":              {"openai/gpt-5.5"},
	"base2-free-gpt-5-5-pro":          {"openai/gpt-5.5-pro"},
	"base2-free-gpt-5-6-luna-pro":     {"openai/gpt-5.6-luna-pro"},
	"base2-free-gpt-5-6-sol":          {"openai/gpt-5.6-sol"},
	"base2-free-gpt-5-6-sol-pro":      {"openai/gpt-5.6-sol-pro"},
	"base2-free-gpt-5-6-terra":        {"openai/gpt-5.6-terra"},
	"base2-free-gpt-5-6-terra-pro":    {"openai/gpt-5.6-terra-pro"},
	"base2-free-gpt-6-1-sol":          {"openai/gpt-6.1-sol"},
	"base2-free-gpt-6-astra":          {"openai/gpt-6-astra"},
	"base2-free-gpt-6-astra-pro":      {"openai/gpt-6-astra-pro"},
	"base2-free-gpt-6-luna-pro":       {"openai/gpt-6-luna-pro"},
	"base2-free-gpt-6-sol":            {"openai/gpt-6-sol"},
	"base2-free-gpt-6-sol-pro":        {"openai/gpt-6-sol-pro"},
	"base2-free-grok-4-20":            {"x-ai/grok-4.20"},
	"base2-free-grok-4-5":             {"x-ai/grok-4.5"},
	"base2-free-grok-4-6":             {"x-ai/grok-4.6"},
	"base2-free-grok-4-7":             {"x-ai/grok-4.7"},
	"base2-free-kimi-k3":              {"moonshotai/kimi-k3"},
	"base2-free-kimi-k3-eco":          {"crof/kimi-k3-eco"},
	"base2-free-llama-4-maverick":     {"meta-llama/llama-4-maverick"},
	"base2-free-luna":                 {"openai/gpt-5.6-luna"},
	"base2-free-luna-6":               {"openai/gpt-6-luna"},
	"base2-free-luna-es":              {"openai/gpt-5.6-luna-es"},
	"base2-free-minimax-m3":           {"minimax/minimax-m3"},
	"base2-free-mistral-large":        {"mistralai/mistral-large"},
	"base2-free-muse-spark":           {"meta/muse-spark-1.2-contributor"},
	"base2-free-muse-spark-1-3":       {"meta/muse-spark-1.3-contributor"},
	"base2-free-mimo":                 {"mimo/mimo-v2.5"},
	"base2-free-mimo-2-6-pro":         {"mimo/mimo-v2.6-pro"},
	"base2-free-o3-pro":               {"openai/o3-pro"},
	"base2-free-ox-alpha":             {"stealth/ox-alpha"},
	"base2-free-qwen3-6-max-preview":  {"qwen/qwen3.6-max-preview"},
	"base2-free-qwen3-6-plus":         {"qwen/qwen3.6-plus"},
	"base2-free-qwen3-7-max":          {"qwen/qwen3.7-max"},
	"base2-free-qwen3-7-plus":         {"qwen/qwen3.7-plus"},
	"base2-free-qwen3-8-27b":          {"qwen/qwen3.8-27b"},
	"base2-free-qwen3-8-flash":        {"qwen/qwen3.8-flash"},
	"base2-free-qwen3-8-max-0902":     {"qwen/qwen3.8-max-0902"},
	"base2-free-qwen3-8-max-prime":    {"qwen/qwen3.8-max-prime"},
	"base2-free-space-bunny-alpha":    {"stealth/space-bunny-alpha"},
	"base3-free-luna-es":              {"openai/gpt-5.6-luna-es"},
	"code-reviewer-deepseek":          {"deepseek/deepseek-v4-pro"},
	"code-reviewer-deepseek-flash":    {"deepseek/deepseek-v4-flash"},
	"code-reviewer-fable":             {"anthropic/claude-fable-5.1"},
	"code-reviewer-gemini-3-8-flash":  {"google/gemini-3.8-flash"},
	"code-reviewer-glm":               {"z-ai/glm-5.2"},
	"code-reviewer-glm-5-3-flash":     {"z-ai/glm-5.3-flash"},
	"code-reviewer-gpt-6-1-sol":       {"openai/gpt-6.1-sol"},
	"code-reviewer-lite":              {"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash"},
	"code-reviewer-luna":              {"openai/gpt-5.6-luna"},
	"code-reviewer-luna-6":            {"openai/gpt-6-luna"},
	"code-reviewer-minimax-m3":        {"minimax/minimax-m3"},
	"code-reviewer-muse-spark":        {"meta/muse-spark-1.2-contributor"},
	"code-reviewer-muse-spark-1-3":    {"meta/muse-spark-1.3-contributor"},
	"code-reviewer-ox-alpha":          {"stealth/ox-alpha"},
	"code-reviewer-space-bunny-alpha": {"stealth/space-bunny-alpha"},
	"file-picker":                     {"google/gemini-2.5-flash-lite"},
	"tmux-cli":                        {"deepseek/deepseek-v4-flash"},
	"tmux-cli-fable":                  {"anthropic/claude-fable-5.1"},
}

// ModelRegistry fetches and caches the agent→model mapping plus per-model
// reasoning-effort ladders from the upstream freebuff constant sources.
type ModelRegistry struct {
	client *http.Client
	logger *log.Logger

	mu             sync.RWMutex
	agentModels    map[string][]string // agentID → []model
	modelToAgent   map[string]string   // model → chosen agentID
	allModels      []string            // deduplicated, sorted
	modelEfforts   map[string][]string // model → allowed reasoning efforts (low→high)
	modelEffortPin map[string]string   // model → pinned reasoning effort
	modelPremium   map[string]bool     // model → premium pool flag
	lastOK         time.Time
	lastError      string
	usingFallback  bool

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewModelRegistry(client *http.Client, logger *log.Logger) *ModelRegistry {
	return &ModelRegistry{
		client:         client,
		logger:         logger,
		agentModels:    make(map[string][]string),
		modelToAgent:   make(map[string]string),
		modelEfforts:   make(map[string][]string),
		modelEffortPin: make(map[string]string),
		modelPremium:   make(map[string]bool),
		stopCh:         make(chan struct{}),
	}
}

func (r *ModelRegistry) Start(ctx context.Context) {
	if err := r.refresh(ctx); err != nil {
		r.logger.Printf("model registry: initial fetch failed, loading hardcoded fallback: %v", err)
		r.loadFallback()
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(modelRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := r.refresh(ctx); err != nil {
					r.mu.RLock()
					lastOK, usingFallback, modelCount := r.lastOK, r.usingFallback, len(r.allModels)
					r.mu.RUnlock()
					source := "upstream"
					if usingFallback {
						source = "fallback"
					}
					age := "never refreshed"
					if !lastOK.IsZero() {
						age = time.Since(lastOK).Round(time.Minute).String()
					}
					r.logger.Printf("model registry: refresh failed (serving %d models from %s snapshot, last good %s): %v", modelCount, source, age, err)
				}
				cancel()
			case <-r.stopCh:
				return
			}
		}
	}()
}

func (r *ModelRegistry) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// Models returns the deduplicated list of all available model names.
func (r *ModelRegistry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.allModels))
	copy(out, r.allModels)
	return out
}

// HasModel checks if the given model is available.
func (r *ModelRegistry) HasModel(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.modelToAgent[model]
	return ok
}

// AgentForModel returns the agent ID that should serve the given model.
func (r *ModelRegistry) AgentForModel(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.modelToAgent[model]
	return agent, ok
}

// AgentIDs returns the list of all known agent IDs.
func (r *ModelRegistry) AgentIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.agentModels))
	for id := range r.agentModels {
		ids = append(ids, id)
	}
	return ids
}

// DefaultModel returns the model used when a client omits one.
func (r *ModelRegistry) DefaultModel() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.modelToAgent[defaultServingModelID]; ok {
		return defaultServingModelID
	}
	if len(r.allModels) > 0 {
		return r.allModels[0]
	}
	return defaultServingModelID
}

// Status describes registry health for /healthz.
type registryStatus struct {
	Source        string    `json:"source"`
	LastRefresh   time.Time `json:"last_refresh,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Agents        int       `json:"agents"`
	Models        int       `json:"models"`
	PremiumModels int       `json:"premium_models"`
}

func (r *ModelRegistry) Status() registryStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status := registryStatus{
		Source: "upstream",
		Agents: len(r.agentModels),
		Models: len(r.allModels),
	}
	if r.usingFallback {
		status.Source = "fallback"
	}
	if !r.lastOK.IsZero() {
		status.LastRefresh = r.lastOK
	}
	status.LastError = r.lastError
	for _, premium := range r.modelPremium {
		if premium {
			status.PremiumModels++
		}
	}
	return status
}

// ClampReasoningEffort narrows a client-requested reasoning effort to the
// ladder the upstream catalog publishes for the model. Models with a fixed
// effort pin always run at that pin; unknown models pass through unchanged.
func (r *ModelRegistry) ClampReasoningEffort(model, effort string) string {
	if effort == "" || effort == "auto" || effort == "none" {
		return effort
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if pin, ok := r.modelEffortPin[model]; ok && pin != "" {
		return pin
	}
	ladder, ok := r.modelEfforts[model]
	if !ok || len(ladder) == 0 {
		return effort
	}
	for _, rung := range ladder {
		if rung == effort {
			return effort
		}
	}

	requestedRank, known := reasoningEffortRank(effort)
	if !known {
		return ladder[0]
	}
	best := ""
	bestRank := -1
	for _, rung := range ladder {
		rungRank, ok := reasoningEffortRank(rung)
		if !ok || rungRank > requestedRank {
			continue
		}
		if rungRank > bestRank {
			best, bestRank = rung, rungRank
		}
	}
	if best == "" {
		// Requested below every rung: run at the ladder floor.
		return ladder[0]
	}
	return best
}

func reasoningEffortRank(effort string) (int, bool) {
	switch effort {
	case "minimal":
		return 0, true
	case "low":
		return 1, true
	case "medium":
		return 2, true
	case "high":
		return 3, true
	case "xhigh":
		return 4, true
	case "max":
		return 5, true
	case "ultra":
		return 6, true
	default:
		return -1, false
	}
}

func (r *ModelRegistry) refresh(ctx context.Context) error {
	modelIDsSource, err := fetchUpstreamSource(ctx, r.client, upstreamModelIDsFile)
	if err != nil {
		return fmt.Errorf("fetch model-ids source: %w", err)
	}
	modelsSource, err := fetchUpstreamSource(ctx, r.client, upstreamModelsFile)
	if err != nil {
		return fmt.Errorf("fetch models source: %w", err)
	}
	agentsSource, err := fetchUpstreamSource(ctx, r.client, upstreamAgentsFile)
	if err != nil {
		return fmt.Errorf("fetch agents source: %w", err)
	}
	// model-config.ts backs constants that are member expressions
	// (e.g. FREEBUFF_MIMO_V25_MODEL_ID = mimoModels.mimoV25).
	modelConfigSource, cfgErr := fetchUpstreamSource(ctx, r.client, upstreamModelCfgFile)

	constants := parseModelIDConstants(modelIDsSource)
	mergeStringConstants(constants, parseModelIDConstants(modelsSource))
	if cfgErr == nil {
		mergeStringConstants(constants, parseMemberExprConstants(modelsSource, parseConfigValues(modelConfigSource)))
	}
	ladders := parseEffortLadders(modelsSource)
	catalog := parseModelCatalog(modelsSource, constants, ladders)

	all := parseAllFreeModels(agentsSource, constants)
	if len(all) == 0 {
		return fmt.Errorf("no free agents found in agents source")
	}

	modelToAgent, allModels := buildModelMapping(all)

	r.mu.Lock()
	r.agentModels = all
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.modelEfforts = make(map[string][]string, len(catalog))
	r.modelEffortPin = make(map[string]string, len(catalog))
	r.modelPremium = make(map[string]bool, len(catalog))
	for modelID, entry := range catalog {
		if len(entry.efforts) > 0 {
			r.modelEfforts[modelID] = entry.efforts
		}
		if entry.effortPin != "" {
			r.modelEffortPin[modelID] = entry.effortPin
		}
		r.modelPremium[modelID] = entry.premium
	}
	r.lastOK = time.Now()
	r.lastError = ""
	r.usingFallback = false
	r.mu.Unlock()

	r.logger.Printf("model registry: updated %d agents, %d models (%d premium, %d with effort ladders)", len(all), len(allModels), r.countPremium(), len(catalog))
	return nil
}

func (r *ModelRegistry) countPremium() int {
	count := 0
	for _, premium := range r.modelPremium {
		if premium {
			count++
		}
	}
	return count
}

func (r *ModelRegistry) loadFallback() {
	modelToAgent, allModels := buildModelMapping(hardcodedFallback)

	r.mu.Lock()
	r.agentModels = hardcodedFallback
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.lastError = "using hardcoded fallback"
	r.usingFallback = true
	r.mu.Unlock()

	r.logger.Printf("model registry: loaded fallback models: %v", allModels)
}

// fetchUpstreamSource reads one upstream constants file, falling back to the
// jsDelivr mirror when raw.githubusercontent is unreachable.
func fetchUpstreamSource(ctx context.Context, client *http.Client, file string) (string, error) {
	var lastErr error
	for _, base := range []string{rawGitHubBase, jsDelivrBase} {
		requestURL := base + upstreamConstantsPath + file
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return "", fmt.Errorf("create request for %s: %w", file, err)
		}
		req.Header.Set("Accept", "text/plain")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("fetch %s: %w", requestURL, err)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read %s: %w", requestURL, readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("fetch %s: unexpected status %d", requestURL, resp.StatusCode)
			continue
		}
		return string(body), nil
	}
	return "", lastErr
}

var (
	modelIDConstantPattern = regexp.MustCompile(`(?:export\s+)?const\s+([A-Z][A-Za-z0-9_]*(?:_MODEL_ID|_REASONING_EFFORT))\s*=\s*'([^']+)'`)
	memberExprPattern      = regexp.MustCompile(`(?:export\s+)?const\s+(FREEBUFF_\w+_MODEL_ID)\s*=\s*([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)`)
	configValuePattern     = regexp.MustCompile(`(\w+):\s*'([^']+)'`)
	agentSetPattern        = regexp.MustCompile(`'([^']+)':\s*new\s+Set\(\[([^\]]*)\]`)
	quotedOrBarePattern    = regexp.MustCompile(`'([^']+)'|([A-Za-z_][A-Za-z0-9_]*)`)
	ladderPattern          = regexp.MustCompile(`(?s)const\s+(\w+)\s*=\s*\[([^\]]*)\]`)
	effortItemPattern      = regexp.MustCompile(`'([a-z]+)'`)
	catalogObjectPattern   = regexp.MustCompile(`(?s)const\s+\w+_MODEL\s*=\s*\{(.*?)\n\}`)
)

// parseModelIDConstants extracts `const X_MODEL_ID = 'vendor/model'` (and
// fixed `X_REASONING_EFFORT = 'high'` pins) into a name→value table.
func parseModelIDConstants(source string) map[string]string {
	out := make(map[string]string)
	for _, match := range modelIDConstantPattern.FindAllStringSubmatch(source, -1) {
		out[match[1]] = match[2]
	}
	return out
}

// parseConfigValues extracts the flat `key: 'value'` table from
// model-config.ts.
func parseConfigValues(source string) map[string]string {
	out := make(map[string]string)
	for _, match := range configValuePattern.FindAllStringSubmatch(source, -1) {
		out[match[1]] = match[2]
	}
	return out
}

// parseMemberExprConstants resolves `const X_MODEL_ID = table.key` member
// expressions against the model-config value table.
func parseMemberExprConstants(source string, configValues map[string]string) map[string]string {
	out := make(map[string]string)
	for _, match := range memberExprPattern.FindAllStringSubmatch(source, -1) {
		if value, ok := configValues[match[3]]; ok {
			out[match[1]] = value
		}
	}
	return out
}

func mergeStringConstants(target, extra map[string]string) {
	for key, value := range extra {
		target[key] = value
	}
}

// parseEffortLadders extracts `const NAME = ['low', 'high', ...]` arrays whose
// elements are all reasoning-effort rungs.
func parseEffortLadders(source string) map[string][]string {
	effortWords := map[string]bool{
		"minimal": true, "low": true, "medium": true,
		"high": true, "xhigh": true, "max": true, "ultra": true,
	}
	out := make(map[string][]string)
	for _, match := range ladderPattern.FindAllStringSubmatch(source, -1) {
		items := effortItemPattern.FindAllStringSubmatch(match[2], -1)
		if len(items) == 0 {
			continue
		}
		ladder := make([]string, 0, len(items))
		allEfforts := true
		for _, item := range items {
			if !effortWords[item[1]] {
				allEfforts = false
				break
			}
			ladder = append(ladder, item[1])
		}
		if allEfforts {
			out[match[1]] = ladder
		}
	}
	return out
}

type catalogModelEntry struct {
	efforts   []string
	effortPin string
	premium   bool
}

// parseModelCatalog extracts per-model effort ladders, effort pins, and the
// premium flag from the catalog objects in freebuff-models.ts.
func parseModelCatalog(source string, constants map[string]string, ladders map[string][]string) map[string]catalogModelEntry {
	out := make(map[string]catalogModelEntry)
	for _, match := range catalogObjectPattern.FindAllStringSubmatch(source, -1) {
		body := match[1]

		idMatch := regexp.MustCompile(`id:\s*([A-Za-z_][A-Za-z0-9_]*|'[^']+')`).FindStringSubmatch(body)
		if idMatch == nil {
			continue
		}
		modelID := resolveConstantToken(idMatch[1], constants)
		if modelID == "" {
			continue
		}

		var entry catalogModelEntry
		if effortsMatch := regexp.MustCompile(`efforts:\s*(\w+|\[)`).FindStringSubmatch(body); effortsMatch != nil {
			token := effortsMatch[1]
			if token == "[" {
				// Inline array; find its closing bracket contents.
				start := strings.Index(body, "efforts:")
				if end := strings.Index(body[start:], "]"); end >= 0 {
					inner := body[start : start+end]
					entry.efforts = extractEffortWords(inner)
				}
			} else if ladder, ok := ladders[token]; ok {
				entry.efforts = ladder
			}
		}
		if pinMatch := regexp.MustCompile(`reasoningEffort:\s*(\w+|'[^']+')`).FindStringSubmatch(body); pinMatch != nil {
			entry.effortPin = resolveConstantToken(pinMatch[1], constants)
		}
		if premiumMatch := regexp.MustCompile(`premium:\s*(true|false)`).FindStringSubmatch(body); premiumMatch != nil {
			entry.premium = premiumMatch[1] == "true"
		}

		out[modelID] = entry
	}
	return out
}

func extractEffortWords(chunk string) []string {
	effortWords := map[string]bool{
		"minimal": true, "low": true, "medium": true,
		"high": true, "xhigh": true, "max": true, "ultra": true,
	}
	var out []string
	for _, item := range effortItemPattern.FindAllStringSubmatch(chunk, -1) {
		if effortWords[item[1]] {
			out = append(out, item[1])
		}
	}
	return out
}

// resolveConstantToken maps an identifier or quoted literal to its value via
// the constants table; quoted literals pass through, unknown identifiers
// resolve to "".
func resolveConstantToken(token string, constants map[string]string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 2 && strings.HasPrefix(token, "'") && strings.HasSuffix(token, "'") {
		return token[1 : len(token)-1]
	}
	return constants[token]
}

// parseAllFreeModels extracts the agent→models mappings from free-agents.ts,
// resolving model-id constants through the catalog's constant table.
func parseAllFreeModels(source string, constants map[string]string) map[string][]string {
	result := make(map[string][]string)
	for _, match := range agentSetPattern.FindAllStringSubmatch(source, -1) {
		agentID := match[1]

		var models []string
		for _, token := range quotedOrBarePattern.FindAllStringSubmatch(match[2], -1) {
			if token[1] != "" {
				models = append(models, token[1])
				continue
			}
			if model, ok := constants[token[2]]; ok && model != "" {
				models = append(models, model)
			}
		}
		if len(models) > 0 {
			result[agentID] = dedupeStrings(models)
		}
	}
	return result
}

// buildModelMapping creates the model→agent reverse mapping and deduplicated
// model list. When a model appears in multiple agents, one is chosen at random.
func buildModelMapping(agentModels map[string][]string) (map[string]string, []string) {
	modelAgents := make(map[string][]string)
	for agentID, models := range agentModels {
		for _, model := range models {
			modelAgents[model] = append(modelAgents[model], agentID)
		}
	}

	modelToAgent := make(map[string]string, len(modelAgents))
	allModels := make([]string, 0, len(modelAgents))
	for model, agents := range modelAgents {
		modelToAgent[model] = agents[rand.Intn(len(agents))]
		allModels = append(allModels, model)
	}
	sort.Strings(allModels)
	return modelToAgent, allModels
}

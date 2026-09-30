package main

import (
	"os"
	"reflect"
	"testing"
)

const fixtureDir = "/tmp/freebuff-ref"

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(fixtureDir + "/" + name)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("fixture %s not present", name)
		}
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func TestParseAllFreeModelsResolvesConstants(t *testing.T) {
	constants := map[string]string{
		"FREEBUFF_DEEPSEEK_V4_PRO_MODEL_ID": "deepseek/deepseek-v4-pro",
	}
	source := `
const AGENT_MODELS = {
  'base2-free-deepseek': new Set([FREEBUFF_DEEPSEEK_V4_PRO_MODEL_ID]),
  'base2-free-inline': new Set(['vendor/inline']),
  'base2-free-unknown': new Set([FREEBUFF_NOT_A_TABLE_ENTRY]),
}
`
	got := parseAllFreeModels(source, constants)
	want := map[string][]string{
		"base2-free-deepseek": {"deepseek/deepseek-v4-pro"},
		"base2-free-inline":   {"vendor/inline"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAllFreeModels = %v, want %v", got, want)
	}
}

func TestParseModelIDConstantsAndMemberExprs(t *testing.T) {
	models := "export const FREEBUFF_GLM_V52_MODEL_ID = 'z-ai/glm-5.2'"
	consts := parseModelIDConstants(models)
	if consts["FREEBUFF_GLM_V52_MODEL_ID"] != "z-ai/glm-5.2" {
		t.Fatalf("constants = %v", consts)
	}

	modelsSource := "export const FREEBUFF_MIMO_V25_MODEL_ID = mimoModels.mimoV25"
	config := parseConfigValues("const x = {\n  mimoV25: 'mimo/mimo-v2.5',\n}")
	resolved := parseMemberExprConstants(modelsSource, config)
	if resolved["FREEBUFF_MIMO_V25_MODEL_ID"] != "mimo/mimo-v2.5" {
		t.Fatalf("member expr constants = %v", resolved)
	}
}

func TestParseEffortLadders(t *testing.T) {
	source := `
export const EFFORTS_THROUGH_HIGH = ['low', 'medium', 'high'] as const
const GLM_V53_FLASH_REASONING_EFFORTS = [
  'low',
  'high',
  'max',
] as const
const NOT_A_LADDER = ['alpha', 'beta'] as const
`
	ladders := parseEffortLadders(source)
	if !reflect.DeepEqual(ladders["EFFORTS_THROUGH_HIGH"], []string{"low", "medium", "high"}) {
		t.Fatalf("EFFORTS_THROUGH_HIGH = %v", ladders["EFFORTS_THROUGH_HIGH"])
	}
	if !reflect.DeepEqual(ladders["GLM_V53_FLASH_REASONING_EFFORTS"], []string{"low", "high", "max"}) {
		t.Fatalf("GLM ladder = %v", ladders["GLM_V53_FLASH_REASONING_EFFORTS"])
	}
	if _, ok := ladders["NOT_A_LADDER"]; ok {
		t.Fatalf("non-effort array parsed as ladder: %v", ladders)
	}
}

func TestClampReasoningEffort(t *testing.T) {
	registry := NewModelRegistry(nil, discardLogger())
	registry.modelEfforts = map[string][]string{
		"deepseek/deepseek-v4-flash": {"low", "high", "max"},
		"z-ai/glm-5.3-flash":         {"low", "high"},
	}
	registry.modelEffortPin = map[string]string{
		"pinned/model": "high",
	}

	cases := []struct {
		model, effort, want string
	}{
		{"deepseek/deepseek-v4-flash", "high", "high"},   // on ladder
		{"deepseek/deepseek-v4-flash", "xhigh", "high"},  // clamps down
		{"deepseek/deepseek-v4-flash", "minimal", "low"}, // below ladder floor
		{"deepseek/deepseek-v4-flash", "weird", "low"},   // unknown effort → floor
		{"pinned/model", "max", "high"},                  // pin wins
		{"pinned/model", "auto", "high"},                 // pin beats sentinels
		{"pinned/model", "none", "high"},                 // pin beats sentinels
		{"unknown/model", "medium", "medium"},            // no ladder → passthrough
		{"deepseek/deepseek-v4-flash", "auto", "auto"},   // sentinels untouched
		{"deepseek/deepseek-v4-flash", "none", "none"},
		{"deepseek/deepseek-v4-flash", "", ""},
	}
	for _, tc := range cases {
		if got := registry.ClampReasoningEffort(tc.model, tc.effort); got != tc.want {
			t.Errorf("ClampReasoningEffort(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
		}
	}
}

// TestParseLiveUpstreamSources validates the whole pipeline against real
// snapshots of the upstream constant files when they are available locally.
func TestParseLiveUpstreamSources(t *testing.T) {
	agents := loadFixture(t, "free-agents.ts")
	models := loadFixture(t, "freebuff-models.ts")
	modelIDs := loadFixture(t, "freebuff-model-ids.ts")

	constants := parseModelIDConstants(modelIDs)
	mergeStringConstants(constants, parseModelIDConstants(models))
	if config, err := os.ReadFile(fixtureDir + "/model-config.ts"); err == nil {
		mergeStringConstants(constants, parseMemberExprConstants(models, parseConfigValues(string(config))))
	}
	if len(constants) < 50 {
		t.Fatalf("only %d constants parsed, expected the full catalog", len(constants))
	}

	ladders := parseEffortLadders(models)
	all := parseAllFreeModels(agents, constants)
	if len(all) < 60 {
		t.Fatalf("parsed %d agents, expected ~79", len(all))
	}
	if models, ok := all["base2-free"]; ok {
		found := map[string]bool{}
		for _, model := range models {
			found[model] = true
		}
		for _, want := range []string{"minimax/minimax-m3", "openai/gpt-5.6-luna", "deepseek/deepseek-v4-flash"} {
			if !found[want] {
				t.Errorf("base2-free missing %q (has %v)", want, models)
			}
		}
	} else {
		t.Errorf("base2-free agent missing from parse result")
	}
	if models, ok := all["base2-free-mimo"]; !ok || !reflect.DeepEqual(models, []string{"mimo/mimo-v2.5"}) {
		t.Errorf("base2-free-mimo = %v, want [mimo/mimo-v2.5] (member-expr resolution)", models)
	}
	if len(ladders) < 3 {
		t.Errorf("only %d effort ladders parsed", len(ladders))
	}
}

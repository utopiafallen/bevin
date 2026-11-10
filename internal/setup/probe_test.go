package setup

import (
	"encoding/json"
	"testing"

	"github.com/utopiafallen/alcyona-agent/internal/provider"
)

// classifyFromJSON unmarshals a /models list and runs it through classify, so
// the tests exercise both the JSON shape and the detection logic.
func classifyFromJSON(t *testing.T, raw string) (string, []provider.ModelInfo) {
	t.Helper()
	var pl probeList
	if err := json.Unmarshal([]byte(raw), &pl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return classify(&pl)
}

func TestClassifyAnthropic(t *testing.T) {
	// Anthropic's /v1/models list carries first_id / last_id / has_more and
	// per-model max_input_tokens.
	kind, models := classifyFromJSON(t, `{"data":[{"id":"claude-3-5-sonnet-20241022","max_input_tokens":200000}],"has_more":false,"first_id":"claude-3-5-sonnet-20241022","last_id":"claude-3-5-haiku-20241022"}`)
	if kind != "anthropic" {
		t.Fatalf("kind = %q, want anthropic", kind)
	}
	if len(models) != 1 || models[0].ID != "claude-3-5-sonnet-20241022" || models[0].MaxContext != 200000 {
		t.Fatalf("models = %+v", models)
	}
}

func TestClassifyLlama(t *testing.T) {
	// llama.cpp's /v1/models items carry an architecture block + meta.n_ctx
	// (the context length), and no first_id / last_id / has_more.
	kind, models := classifyFromJSON(t, `{"object":"list","data":[{"id":"LiquidAI/LFM3-VL-2B-GGUF:Q8_0","architecture":{"input_modalities":["text","image","video"],"output_modalities":["text"]},"meta":{"n_ctx":128000,"n_embd":2048}}]}`)
	if kind != "llama" {
		t.Fatalf("kind = %q, want llama", kind)
	}
	if len(models) != 1 || models[0].MaxContext != 128000 || len(models[0].InputModalities) != 3 {
		t.Fatalf("models = %+v", models)
	}
}

func TestClassifyOpenAI(t *testing.T) {
	// OpenAI's /v1/models list is {object:"list", data:[{id,object}]} with no
	// architecture / first_id. Context lengths come from the best-effort map.
	kind, models := classifyFromJSON(t, `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"some-unknown-model","object":"model"}]}`)
	if kind != "openai" {
		t.Fatalf("kind = %q, want openai", kind)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].MaxContext != 128000 {
		t.Fatalf("gpt-4o MaxContext = %d, want 128000", models[0].MaxContext)
	}
	if models[1].MaxContext != 0 {
		t.Fatalf("unknown MaxContext = %d, want 0", models[1].MaxContext)
	}
}

func TestClassifyEmpty(t *testing.T) {
	kind, _ := classifyFromJSON(t, `{"data":[]}`)
	if kind != "" {
		t.Fatalf("kind = %q, want empty for a list with no models", kind)
	}
}

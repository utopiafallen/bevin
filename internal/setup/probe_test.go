package setup

import (
	"testing"

	"github.com/utopiafallen/bevin/internal/provider"
)

// classifyRaw runs classify on a raw /models JSON body.
func classifyRaw(t *testing.T, raw string) (string, []provider.ModelInfo, error) {
	t.Helper()
	return classify([]byte(raw))
}

func TestClassifyAnthropic(t *testing.T) {
	// Anthropic's /v1/models list carries first_id / last_id / has_more and
	// per-model max_input_tokens.
	kind, models, err := classifyRaw(t, `{"data":[{"id":"claude-3-5-sonnet-20241022","max_input_tokens":200000}],"has_more":false,"first_id":"claude-3-5-sonnet","last_id":"claude-3-haiku"}`)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
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
	kind, models, err := classifyRaw(t, `{"object":"list","data":[{"id":"LiquidAI/LFM3-VL-2B-GGUF:Q8_0","architecture":{"input_modalities":["text","image","video"],"output_modalities":["text"]},"meta":{"n_ctx":128000,"n_embd":2048}}]}`)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
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
	kind, models, err := classifyRaw(t, `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"some-unknown-model","object":"model"}]}`)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
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
	_, _, err := classifyRaw(t, `{"data":[]}`)
	if err == nil {
		t.Fatalf("want an error for a list with no models")
	}
}

func TestClassifyContextForgiving(t *testing.T) {
	// n_ctx at the top level of the model (not in a meta block).
	_, models, err := classifyRaw(t, `{"data":[{"id":"m","n_ctx":32768}]}`)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if models[0].MaxContext != 32768 {
		t.Fatalf("MaxContext = %d, want 32768", models[0].MaxContext)
	}
	// context_length nested several levels deep.
	_, models2, err := classifyRaw(t, `{"data":[{"id":"m","limits":{"window":{"context_length":8192}}}]}`)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if models2[0].MaxContext != 8192 {
		t.Fatalf("MaxContext = %d, want 8192", models2[0].MaxContext)
	}
}

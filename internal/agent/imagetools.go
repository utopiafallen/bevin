// Image perception via the vision aux model (the aux-model perception loop,
// stage 1: chat attachments).
//
// The user attaches one or more images to a chat message (the web UI's
// attachment controls — shown only while /api/models reports a vision aux).
// The images ride the turn request as in-memory bytes (TurnImage); nothing
// about the images is persisted — the persistent artifact is the description
// text the vision model produces, which rides back on the tool result's P7
// row like any other tool result (the P3/P7 model is unchanged).
//
// The image_analyze tool is registered unconditionally like the other
// tools, but wireTools OFFERS it only while the live config has a
// vision-capable aux model (the npCfg pattern: an admin save applies from
// the next turn, no restart). A stray call that still lands (the model
// imitating it from its own history after an admin removed the aux) gets a
// structured "no vision model" result, not a crash.
//
// Execution: a perception request is the same unit of work in the
// scheduler's per-model pools (the aux pool-slots decision) — it submits to
// the aux's pool id. ONE exception: when the aux EXACTLY matches the
// conversation's active model, that pool is the one the turn itself holds a
// slot in, and on a 1-slot pool the perception would queue behind the
// turn's own held slot forever (a self-deadlock). In that case the
// perception runs INLINE in the turn: the turn's slot already accounts
// this user's work against the pool's budget, so running inside it is the
// liveness-preserving reading of the shared bound.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/provider"
)

// TurnImage is one image attached to the user's current message, for the
// duration of the turn. The bytes live in memory only (see the file
// header); Name + MimeType ride the result text and the UI's optimistic
// render.
type TurnImage struct {
	Name     string
	MimeType string
	Data     []byte
}

// ctxTurnImagesKey carries the turn's attached images to the tool executors
// (harness plumbing, the ctxConversationKey pattern — memorytools.go).
type ctxTurnImagesKeyT struct{}

var ctxTurnImagesKey = ctxTurnImagesKeyT{}

// turnImagesFromContext returns the turn's attached images (nil = none).
func turnImagesFromContext(ctx context.Context) []TurnImage {
	v, _ := ctx.Value(ctxTurnImagesKey).([]TurnImage)
	return v
}

// attachedImagesMarker is the transcript text of an image-only message (the
// bytes are never persisted — the image_analyze tool result is the record).
func attachedImagesMarker(n int) string {
	if n == 1 {
		return "(1 image attached)"
	}
	return fmt.Sprintf("(%d images attached)", n)
}

// visionAux returns the first aux model configured to perceive images,
// read from LIVE config (the npCfg pattern: an admin save applies from the
// next turn, no restart). The first configured vision aux wins — the model
// sees one perception surface (v1).
func (a *Agent) visionAux() (config.AuxModel, bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, x := range a.cfg.Aux {
		for _, k := range x.Kinds {
			if k == "vision" {
				return x, true
			}
		}
	}
	return config.AuxModel{}, false
}

// HasVisionAux reports whether the live config has a vision-capable aux
// model. The /api/models "vision" flag (and with it the web UI's attachment
// controls) renders from it, and wireTools offers image_analyze while it is
// true.
func (a *Agent) HasVisionAux() bool {
	_, ok := a.visionAux()
	return ok
}

// perceptionSystemPrompt is the vision aux's job: turn one image into the
// complete text record the assistant model will have of it.
const perceptionSystemPrompt = `You are the perception model behind Bevin, a personal assistant. Describe this image in precise, factual detail: the main subjects and what they are doing, ALL visible text (transcribe it verbatim, in its original language), UI elements with their values and states, and layout where it matters. Do not speculate about anything that is not visible. Your description is shown to the assistant model as the only record of this image, so make it complete enough to answer questions about the image without seeing it.`

// registerImageAnalyzeTool wires the image_analyze tool into the registry
// (called from New; the per-turn offer is the wireTools filter).
func registerImageAnalyzeTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "image_analyze",
		Description: "Analyze an image the user attached to their current message — the vision model returns a detailed text description (subjects, visible text transcribed, UI values). This is how you 'see' an attached image: call it once per image you need (image = the attachment number, 1 = the first). If the message's text is clear enough on its own, you don't need to call it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"image": map[string]any{
					"type":        "integer",
					"description": "which attachment to analyze (1 = the first attached image); default 1",
				},
				"focus": map[string]any{
					"type":        "string",
					"description": "optional: what to pay special attention to (e.g. 'the error message text')",
				},
			},
		},
		Execute: a.imageAnalyze,
	})
}

// imageAnalyzeArgs is one image_analyze call's arguments (the model's JSON).
type imageAnalyzeArgs struct {
	Image *int   `json:"image"`
	Focus string `json:"focus"`
}

// imageAnalyze runs one perception request: it sends the named attachment
// to the vision aux model and returns the model's description as the tool
// result (the P7 row). Every failure mode is a result for the model, never
// a turn error (the executeToolCall contract).
func (a *Agent) imageAnalyze(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ia imageAnalyzeArgs
	if err := json.Unmarshal(args, &ia); err != nil {
		return "", fmt.Errorf("image_analyze arguments: %v — pass {\"image\": <attachment number>, \"focus\": \"<optional>\"}", err)
	}
	images := turnImagesFromContext(ctx)
	if len(images) == 0 {
		return "no images are attached to this message — the user sent text only. Answer from the text, or ask the user to attach an image.", nil
	}
	n := 1
	if ia.Image != nil {
		n = *ia.Image
	}
	if n < 1 || n > len(images) {
		return fmt.Sprintf("attachment %d does not exist — this message has %d image(s). Use one of 1..%d.", n, len(images), len(images)), nil
	}
	img := images[n-1]
	aux, ok := a.visionAux()
	if !ok {
		return "no vision model is configured right now, so the image cannot be analyzed — answer from the text, or ask the user to try again later.", nil
	}
	// Pool choice (see the file header): the perception request submits to
	// the aux's pool id — EXCEPT the self-deadlock case (the aux exactly
	// matches the conversation's active model and that pool has 1 slot):
	// there the turn holds the pool's only slot, so the perception runs
	// inline in the turn instead.
	run := func(ctx context.Context) (string, error) {
		return a.perceiveImage(ctx, aux, img, ia.Focus)
	}
	var (
		desc string
		err  error
	)
	convID := conversationFromContext(ctx)
	inline := convID == ""
	if !inline {
		us, uerr := a.userStore(handle)
		if uerr != nil {
			inline = true
		} else if conv, cerr := us.GetConversation(convID); cerr != nil {
			inline = true
		} else if aux.PoolID == conv.ActiveModelID && a.modelSlots(aux.PoolID) == 1 {
			inline = true // the turn holds the pool's only slot (see above)
		} else {
			err = a.sched.Submit(ctx, JobInfo{Handle: handle, ConvID: convID, ModelID: aux.PoolID}, func(jobCtx context.Context) error {
				desc, err = run(jobCtx)
				return err
			})
		}
	}
	if inline {
		desc, err = run(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("the vision model call failed: %v", err)
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "", fmt.Errorf("the vision model returned an empty description")
	}
	label := img.Name
	if label == "" {
		label = "the attached image"
	}
	return fmt.Sprintf("Description of %s:\n\n%s", label, desc), nil
}

// perceiveImage sends one image to the vision aux model — a plain
// non-streaming Chat call with the perception system prompt + the image —
// and returns the model's description. The aux's profile is not consumed
// here (v1: perception is a plain description call — no tools, no
// reasoning-effort).
func (a *Agent) perceiveImage(ctx context.Context, aux config.AuxModel, img TurnImage, focus string) (string, error) {
	p, err := provider.New(aux.Provider, aux.BaseURL, aux.APIKey)
	if err != nil {
		return "", fmt.Errorf("building the vision provider: %w", err)
	}
	system := perceptionSystemPrompt
	if f := strings.TrimSpace(focus); f != "" {
		system += "\n\nThe assistant is especially interested in: " + f
	}
	req := provider.ChatRequest{
		System: []provider.ContentPart{{Text: system}},
		Messages: []provider.Message{{
			Role:  provider.RoleUser,
			Parts: []provider.ContentPart{{Text: "Analyze this image."}, {Media: &provider.Media{Kind: provider.MediaImage, MimeType: img.MimeType, Data: img.Data}}},
		}},
	}
	res, err := p.Chat(ctx, aux.ModelID, req)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

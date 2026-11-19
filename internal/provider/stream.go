package provider

import (
	"bufio"
	"io"
	"sort"
	"strings"
)

// oaiStreamChunk is one SSE data line from an OpenAI-compatible
// /chat/completions stream. finish_reason lives per-choice in the standard
// wire; null unmarshals to "".
type oaiStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string              `json:"content"`
			ReasoningContent string              `json:"reasoning_content"`
			ToolCalls        []oaiStreamToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
}

// oaiStreamToolCall is one delta.tool_calls entry. A tool call's id + name
// arrive on its first fragment (some backends repeat them; the first
// non-empty wins) and its JSON arguments usually arrive split across the
// following fragments (each carries the next piece — OpenAI's style;
// llama.cpp tends to send the whole call in one fragment). The accumulator
// below reassembles either shape into complete calls.
type oaiStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiStreamToolAccum is one in-flight streamed tool call.
type oaiStreamToolAccum struct {
	id   string
	name string
	args strings.Builder
}

// oaiToolAccumulator folds the delta.tool_calls fragments of one streaming
// OAI response into complete tool calls. Both OAI-compatible providers
// (openai + llama) share the wire, so they share this accumulator: the
// stream loop Absorbs each chunk (which also yields the per-fragment
// ToolCallDeltas for the emitted Chunk) and calls Result once at the end.
// A response with no tool calls yields a nil slice — exactly the pre-tools
// shape.
type oaiToolAccumulator struct {
	tools map[int]*oaiStreamToolAccum
}

func newOAIToolAccumulator() *oaiToolAccumulator {
	return &oaiToolAccumulator{tools: map[int]*oaiStreamToolAccum{}}
}

// Absorb folds one chunk's tool-call fragments into the in-flight calls and
// returns the fragments (for emission). Text/reasoning stay out of here —
// the providers keep aggregating those directly.
func (a *oaiToolAccumulator) Absorb(chunk oaiStreamChunk) []ToolCallDelta {
	var out []ToolCallDelta
	for _, ch := range chunk.Choices {
		for _, tc := range ch.Delta.ToolCalls {
			acc, ok := a.tools[tc.Index]
			if !ok {
				acc = &oaiStreamToolAccum{}
				a.tools[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
			out = append(out, ToolCallDelta{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return out
}

// Result turns the accumulated calls into complete ToolCalls in call-index
// order (the order the model emitted them).
func (a *oaiToolAccumulator) Result() []ToolCall {
	if len(a.tools) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(a.tools))
	for i := range a.tools {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]ToolCall, 0, len(idxs))
	for _, i := range idxs {
		acc := a.tools[i]
		out = append(out, ToolCall{ID: acc.id, Name: acc.name, Arguments: acc.args.String()})
	}
	return out
}

// newLineScanner returns a buffered line scanner sized for large SSE payloads.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
}

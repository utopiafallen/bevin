package provider

import (
	"bufio"
	"io"
)

// oaiStreamChunk is one SSE data line from an OpenAI-compatible
// /chat/completions stream. finish_reason lives per-choice in the standard
// wire; null unmarshals to "".
type oaiStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
}

// newLineScanner returns a buffered line scanner sized for large SSE payloads.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
}
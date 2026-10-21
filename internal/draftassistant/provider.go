package draftassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// maxResponseBodyBytes bounds how much of the provider's response is read.
// A chat completion with max_tokens 2048 is a few kilobytes; four
// megabytes is a generous ceiling that still stops a broken provider from
// streaming garbage into memory.
const maxResponseBodyBytes = 4 << 20

// errorExcerptBytes bounds the slice of a provider error body quoted in
// fallback reasons: enough to be diagnostic, not enough to drown a CLI
// report (or to smuggle a whole error page into the agent API response).
const errorExcerptBytes = 512

// chatMessage is one entry of the OpenAI chat-completions messages array.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the wire shape of a non-streaming chat-completions call.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

// chatResponse is the subset of the chat-completions response the
// refinement reads. Anything else in the payload is ignored.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// httpClient is shared by every provider call. Timeouts are enforced with
// the request context (section.Timeout), so no per-client Timeout is set.
var httpClient = &http.Client{}

// chatCompletion posts one non-streaming chat-completions request to the
// OpenAI-compatible endpoint named by section (api_base + "/chat/completions")
// and returns the assistant message content. The API key travels only in
// the Authorization header: it is never logged, never echoed in error
// messages (see redact), and never part of any returned value.
func chatCompletion(ctx context.Context, section config.DraftAssistantConfig, apiKey, systemPrompt, userPrompt string) (string, error) {
	if apiKey == "" {
		// Defensive: Refine checks this first; a direct call must not
		// send an empty bearer token either.
		return "", fmt.Errorf("no API key available")
	}

	body, err := json.Marshal(chatRequest{
		Model: section.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens:   section.MaxTokens,
		Temperature: 0,
		Stream:      false,
	})
	if err != nil {
		return "", fmt.Errorf("encoding request: %w", err)
	}

	endpoint := strings.TrimRight(section.APIBase, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return "", fmt.Errorf("reading provider response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, excerpt(payload, errorExcerptBytes))
	}

	var parsed chatResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("decoding provider response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("provider response has no choices")
	}
	content := parsed.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("provider response has an empty message")
	}
	return content, nil
}

// excerpt quotes at most limit bytes of a provider body, single-lined so
// it cannot inject line breaks into reports. The API key is not redacted
// here (it is not in scope); callers redact the final message.
func excerpt(body []byte, limit int) string {
	if len(body) > limit {
		body = body[:limit]
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(body), "\n", " "))
	if text == "" {
		return "(empty body)"
	}
	return text
}

// redact replaces every occurrence of secret with *** inside s. It is
// applied to every error message and fallback reason that can carry
// provider-produced text, so a provider that echoes the Authorization
// header in an error body cannot leak the key into CLI output, agent API
// responses or logs.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

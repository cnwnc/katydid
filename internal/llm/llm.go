// Package llm is a minimal OpenAI-compatible chat client.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client calls one OpenAI-compatible /chat/completions endpoint.
type Client struct {
	base  string
	key   string
	model string
	hc    *http.Client
}

// New trims one trailing "/" from baseURL so both ".../v1" and ".../v1/" work.
func New(baseURL, key, model string) *Client {
	if n := len(baseURL); n > 0 && baseURL[n-1] == '/' {
		baseURL = baseURL[:n-1]
	}
	return &Client{base: baseURL, key: key, model: model, hc: &http.Client{Timeout: 60 * time.Second}}
}

// Enabled reports whether base and model are set; the key is optional
// so keyless local servers count as configured.
func (c *Client) Enabled() bool {
	return c != nil && c.base != "" && c.model != ""
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// ChatJSON asks the model to answer system+user and unmarshals the reply into out.
func (c *Client) ChatJSON(ctx context.Context, system, user string, out any) error {
	reqBody := chatRequest{
		Model:          c.model,
		Messages:       []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		ResponseFormat: responseFormat{Type: "json_object"},
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("llm: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("llm: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("llm: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		clipped := make([]byte, 300)
		n, _ := resp.Body.Read(clipped)
		return fmt.Errorf("llm: status %d: %s", resp.StatusCode, clipped[:n])
	}
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("llm: decode: %w", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return fmt.Errorf("llm: empty response")
	}
	if err := json.Unmarshal([]byte(parsed.Choices[0].Message.Content), out); err != nil {
		return fmt.Errorf("llm: decode: %w", err)
	}
	return nil
}

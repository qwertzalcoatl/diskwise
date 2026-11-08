package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LLM sends one prompt and returns the reply text.
type LLM interface {
	Name() string
	Ask(ctx context.Context, prompt string) (string, error)
}

// pickLLM chooses a provider. DISKWISE_LLM forces one of:
// cmd, anthropic, openai, gemini, claude. Otherwise the first configured
// provider in that order is used.
func pickLLM() (LLM, error) {
	model := os.Getenv("DISKWISE_MODEL")
	choice := os.Getenv("DISKWISE_LLM")
	avail := map[string]LLM{}
	order := []string{}
	add := func(name string, l LLM) {
		avail[name] = l
		order = append(order, name)
	}
	if c := os.Getenv("DISKWISE_LLM_CMD"); c != "" {
		add("cmd", cmdLLM{c})
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		add("anthropic", anthropicLLM{k, or(model, "claude-haiku-4-5")})
	}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		add("openai", openaiLLM{k, or(os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"), or(model, "gpt-4.1-mini")})
	}
	if k := or(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY")); k != "" {
		add("gemini", geminiLLM{k, or(model, "gemini-2.5-flash")})
	}
	if p, err := exec.LookPath("claude"); err == nil {
		add("claude", claudeCLI{p, model})
	}
	if choice != "" {
		if l, ok := avail[choice]; ok {
			return l, nil
		}
		return nil, fmt.Errorf("DISKWISE_LLM=%s is not configured", choice)
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("no LLM configured: set ANTHROPIC_API_KEY, OPENAI_API_KEY (+OPENAI_BASE_URL for Ollama), " +
			"GEMINI_API_KEY, DISKWISE_LLM_CMD, or install the claude CLI")
	}
	return avail[order[0]], nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

func postJSON(ctx context.Context, url string, headers map[string]string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

type anthropicLLM struct{ key, model string }

func (a anthropicLLM) Name() string { return "anthropic/" + a.model }
func (a anthropicLLM) Ask(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Content []struct{ Text string } `json:"content"`
	}
	err := postJSON(ctx, "https://api.anthropic.com/v1/messages",
		map[string]string{"x-api-key": a.key, "anthropic-version": "2023-06-01"},
		map[string]any{"model": a.model, "max_tokens": 800,
			"messages": []map[string]string{{"role": "user", "content": prompt}}}, &out)
	if err != nil {
		return "", err
	}
	if len(out.Content) == 0 {
		return "", fmt.Errorf("empty reply")
	}
	return out.Content[0].Text, nil
}

type openaiLLM struct{ key, base, model string }

func (o openaiLLM) Name() string { return "openai/" + o.model }
func (o openaiLLM) Ask(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	err := postJSON(ctx, strings.TrimRight(o.base, "/")+"/chat/completions",
		map[string]string{"authorization": "Bearer " + o.key},
		map[string]any{"model": o.model, "messages": []map[string]string{{"role": "user", "content": prompt}}}, &out)
	if err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("empty reply")
	}
	return out.Choices[0].Message.Content, nil
}

type geminiLLM struct{ key, model string }

func (g geminiLLM) Name() string { return "gemini/" + g.model }
func (g geminiLLM) Ask(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	err := postJSON(ctx, "https://generativelanguage.googleapis.com/v1beta/models/"+g.model+":generateContent",
		map[string]string{"x-goog-api-key": g.key},
		map[string]any{"contents": []map[string]any{{"parts": []map[string]string{{"text": prompt}}}}}, &out)
	if err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty reply")
	}
	return out.Candidates[0].Content.Parts[0].Text, nil
}

// cmdLLM pipes the prompt to a shell command (e.g. `llm -m x`, `ollama run qwen3`).
type cmdLLM struct{ command string }

func (c cmdLLM) Name() string { return "cmd: " + c.command }
func (c cmdLLM) Ask(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", c.command)
	return runPrompt(cmd, prompt)
}

// claudeCLI uses Claude Code in print mode, run from a temp dir so no
// project instructions are loaded.
type claudeCLI struct{ bin, model string }

func (c claudeCLI) Name() string { return "claude CLI" }
func (c claudeCLI) Ask(ctx context.Context, prompt string) (string, error) {
	args := []string{"-p", "--output-format", "text"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = os.TempDir()
	return runPrompt(cmd, prompt)
}

func runPrompt(cmd *exec.Cmd, prompt string) (string, error) {
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

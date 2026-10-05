package persona

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// completionRequest 是一次补全请求；System/User 分别对应 system 与 user 消息。
type completionRequest struct {
	System      string
	User        contentParts // user 消息的 content 数组（text + 可选 image_url）
	Temperature float64
	MaxTokens   int
}

// contentPart 是 OpenAI content 数组中的一个块：text 或 image_url。
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

// imageURLPart 是 image_url 块的内容。
type imageURLPart struct {
	URL string `json:"url"`
}

// contentParts 是 user 消息的 content（恒为数组）。
type contentParts []contentPart

// textParts 构造只含一个 text 块的 content，供内部与测试使用。
func textParts(s string) contentParts { return contentParts{{Type: "text", Text: s}} }

// completer 屏蔽具体 LLM 服务，便于单测注入桩。
type completer interface {
	Complete(ctx context.Context, req completionRequest) (string, error)
}

type openaiClient struct { // 实现 completer
	baseURL string
	apiKey  string
	model   string
	headers map[string]string
	http    *http.Client
	log     *slog.Logger
	retries int
	timeout time.Duration
	debug   bool // 是否以 Debug 输出 LLM 请求与响应（debug_prompts）
}

// 确保 openaiClient 满足 completer。
var _ completer = (*openaiClient)(nil)

// newOpenAIClient 构造 OpenAI 兼容客户端。
func newOpenAIClient(cfg *config, hc *http.Client, log *slog.Logger) *openaiClient {
	return &openaiClient{
		baseURL: cfg.llmBaseURL,
		apiKey:  cfg.llmAPIKey,
		model:   cfg.llmModel,
		headers: cfg.llmExtraHeaders,
		http:    hc,
		log:     log,
		retries: cfg.llmMaxRetries,
		timeout: cfg.llmTimeout,
		debug:   cfg.debugPrompts,
	}
}

// Complete 调用 chat completions；仅对 429/5xx/网络错误重试。
func (c *openaiClient) Complete(ctx context.Context, req completionRequest) (string, error) {
	attempts := c.retries + 1
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			backoff := 500 * time.Millisecond << (attempt - 1)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		content, retryable, err := c.attempt(ctx, req)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
	}
	return "", lastErr
}

// attempt 执行单次请求；返回是否可重试。
func (c *openaiClient) attempt(ctx context.Context, req completionRequest) (string, bool, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload := map[string]any{
		"model":       c.model,
		"temperature": req.Temperature,
		"max_tokens":  req.MaxTokens,
		"messages": []map[string]any{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", false, fmt.Errorf("persona: 请求编码失败: %w", err)
	}
	if c.debug {
		c.log.Debug("persona llm 请求", "host", hostOf(c.baseURL), "model", c.model, "body", truncateRunes(string(body), 2048))
	}

	endpoint := trimRightSlash(c.baseURL) + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(actx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", false, fmt.Errorf("persona: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// 网络错误可重试；ctx 取消直接失败。
		if ctx.Err() != nil {
			return "", false, err
		}
		return "", true, fmt.Errorf("persona: llm 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", true, fmt.Errorf("persona: 读取响应失败: %w", err)
	}
	if c.debug {
		c.log.Debug("persona llm 响应", "host", hostOf(c.baseURL), "model", c.model, "status", resp.StatusCode, "body", truncateRunes(string(data), 2048))
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", true, fmt.Errorf("persona: llm http %d: %s", resp.StatusCode, snippet(data))
	}
	if resp.StatusCode >= 400 {
		return "", false, fmt.Errorf("persona: llm http %d: %s", resp.StatusCode, snippet(data))
	}
	content, err := parseCompletion(data)
	if err != nil {
		return "", false, err
	}
	return content, false, nil
}

// parseCompletion 解析 choices[0].message.content。
func parseCompletion(data []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("persona: llm 响应解析失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("persona: llm 响应缺少 choices")
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil {
		return "", fmt.Errorf("persona: llm 响应的 content 不是字符串（可能只返回 reasoning_content）")
	}
	return content, nil
}

// snippet 截断响应体片段用于日志。
func snippet(b []byte) string {
	const max = 256
	if len(b) > max {
		b = b[:max]
	}
	return string(b)
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// hostOf 返回 URL 的 host（不含路径与查询），解析失败时返回去空白的原值。
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

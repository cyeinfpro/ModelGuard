package guard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ProbeConfigurationError struct{ Message string }

func (e *ProbeConfigurationError) Error() string { return e.Message }
func configurationError(message string) error    { return &ProbeConfigurationError{Message: message} }

type Identity struct {
	Token, Proxy string
	Headers      http.Header
}
type ResolveIdentity func(context.Context, int64) (Identity, error)
type Probe interface {
	Sample(context.Context, Account, Target, string) (string, error)
}
type SampleProgress struct {
	Phase       string `json:"phase"`
	Events      int    `json:"events"`
	OutputBytes int    `json:"output_bytes"`
}
type ProgressProbe interface {
	SampleWithProgress(context.Context, Account, Target, string, func(SampleProgress)) (string, error)
}
type Prober struct {
	Resolve ResolveIdentity
	Pool    *ClientPool
}

// Transports are shared by proxy, bounded and closed when no longer used.
type ClientPool struct {
	mu      sync.Mutex
	entries map[string]*http.Transport
}

func (p *ClientPool) Client(proxy string) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = map[string]*http.Transport{}
	}
	tr := p.entries[proxy]
	if tr == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		tr.DisableCompression = true
		tr.MaxIdleConns = 16
		tr.MaxIdleConnsPerHost = 4
		tr.IdleConnTimeout = 60 * time.Second
		tr.ResponseHeaderTimeout = 180 * time.Second
		if proxy != "" {
			u, err := url.Parse(proxy)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				return nil, errors.New("代理地址无效")
			}
			tr.Proxy = http.ProxyURL(u)
		}
		if len(p.entries) >= 64 {
			for k, t := range p.entries {
				t.CloseIdleConnections()
				delete(p.entries, k)
				break
			}
		}
		p.entries[proxy] = tr
	}
	return &http.Client{Transport: tr, CheckRedirect: NoRedirect}, nil
}
func (p *ClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.entries {
		t.CloseIdleConnections()
	}
}
func (p *Prober) Sample(ctx context.Context, a Account, t Target, prompt string) (string, error) {
	return p.SampleWithProgress(ctx, a, t, prompt, nil)
}
func (p *Prober) SampleWithProgress(ctx context.Context, a Account, t Target, prompt string, progress func(SampleProgress)) (string, error) {
	report := func(phase string) {
		if progress != nil {
			progress(SampleProgress{Phase: phase})
		}
	}
	report("identity")
	endpoint := "https://chatgpt.com/backend-api/codex/responses"
	id := Identity{Headers: http.Header{}}
	if a.Type == "oauth" {
		if p.Resolve == nil {
			return "", configurationError("宿主不支持账号身份解析")
		}
		var err error
		id, err = p.Resolve(ctx, a.ID)
		if err != nil {
			return "", configurationError("宿主无法解析账号出站身份")
		}
	} else if a.Type == "apikey" {
		var err error
		endpoint, err = apiResponsesURL(credential(a, "base_url"))
		if err != nil {
			return "", configurationError(err.Error())
		}
		id.Token = credential(a, "api_key")
		id.Proxy = a.OutboundProxy
	} else {
		return "", configurationError("账号类型不支持")
	}

	if id.Token == "" {
		return "", configurationError("账号没有可用出站凭据")
	}
	if id.Headers == nil {
		id.Headers = http.Header{}
	}
	model := t.Model
	if a.Type == "apikey" {
		model = mappedModel(a, model)
	}
	payload := map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}, "instructions": "You are a helpful assistant.", "stream": true, "store": false}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return "", errors.New("无法创建探测请求")
	}
	req.Header = id.Headers.Clone()
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if a.Type == "oauth" && req.Header.Get("OpenAI-Beta") == "" {
		req.Header.Set("OpenAI-Beta", "responses=experimental")
	}
	client, err := p.Pool.Client(id.Proxy)
	if err != nil {
		return "", err
	}
	// Each fingerprint sample uses a fresh connection. Do not carry a partially
	// consumed SSE connection (or business traffic transport) into the next sample.
	transport := client.Transport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	client = &http.Client{Transport: transport, CheckRedirect: NoRedirect}
	report("connecting")
	res, err := client.Do(req)
	if err != nil {
		return "", errors.New("上游连接失败或超时")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("上游 HTTP %d", res.StatusCode)
	}
	report("receiving")
	answer, err := readResponsesProgress(res.Body, progress)
	if err != nil && ctx.Err() != nil {
		return "", errors.New("采样超时，未收到完整 response.completed")
	}
	return answer, err
}

// Require response.completed; an EOF, incomplete response, refusal or partial
// sample is never proof of mismatch. Only output_text is scored, not reasoning.
func ReadResponses(body io.Reader) (string, error) { return readResponsesProgress(body, nil) }
func readResponsesProgress(body io.Reader, progress func(SampleProgress)) (string, error) {
	scan := bufio.NewScanner(io.LimitReader(body, 4<<20))
	scan.Buffer(make([]byte, 8192), 1<<20)
	var result strings.Builder
	var data []string
	events := 0
	lastProgress := time.Time{}
	process := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if raw == "[DONE]" {
			return false, nil
		}
		var e struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(raw), &e) != nil {
			return false, errors.New("上游 SSE 数据无效")
		}
		events++
		if progress != nil && time.Since(lastProgress) >= 500*time.Millisecond {
			phase := "receiving"
			if strings.Contains(e.Type, "reasoning") {
				phase = "reasoning"
			}
			if strings.Contains(e.Type, "output_text") {
				phase = "output"
			}
			progress(SampleProgress{Phase: phase, Events: events, OutputBytes: result.Len()})
			lastProgress = time.Now()
		}
		switch e.Type {
		case "response.refusal.delta", "response.refusal.done":
			return false, errors.New("上游拒绝完成采样，不能用于指纹判定")
		case "response.output_text.delta":
			result.WriteString(e.Delta)
			if result.Len() > 128<<10 {
				return false, errors.New("探测输出超出限制")
			}
		case "response.completed":
			if e.Response.Status != "completed" {
				return false, errors.New("上游缺少完成状态")
			}
			// The completed response is authoritative if it contains output.
			// Never score partial deltas when the final response differs.
			var complete strings.Builder
			for _, o := range e.Response.Output {
				for _, c := range o.Content {
					if c.Type == "refusal" {
						return false, errors.New("上游拒绝完成采样，不能用于指纹判定")
					}
					if c.Type == "output_text" {
						complete.WriteString(c.Text)
					}
				}
			}
			if complete.Len() > 0 {
				result = complete
			}

			if result.Len() == 0 || result.Len() > 128<<10 {
				return false, errors.New("探测输出为空或超出限制")
			}
			return true, nil
		case "response.failed", "response.incomplete", "error":
			return false, errors.New("上游响应未完整完成")
		}
		return false, nil
	}
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			done, err := process()
			if err != nil {
				return "", err
			}
			if done {
				return result.String(), nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scan.Err(); err != nil {
		return "", errors.New("上游流中断或超过限制")
	}
	done, err := process()
	if err != nil {
		return "", err
	}
	if done {
		return result.String(), nil
	}
	return "", errors.New("上游流缺少 response.completed")
}

func Evaluate(ctx context.Context, account Account, t Target, c Config, probe Probe, detector *fingerprint.Detector, reserve func(bool) error) (*fingerprint.Analysis, string, error) {
	samples := make([]fingerprint.Output, 0, 3)
	for index, ch := range newFingerprintChallenges() {
		var failure string
		for attempt := 1; attempt <= 3; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			if attempt > 1 {
				delay := time.Duration(attempt-1) * time.Second
				if observer, ok := probe.(retryObserver); ok {
					if err := observer.Retry(ctx, account, index+1, attempt, failure, delay); err != nil {
						return nil, "", err
					}
				} else if err := waitRetry(ctx, delay); err != nil {
					return nil, "", err
				}
			}
			if err := reserve(attempt > 1); err != nil {
				return nil, "", err
			}
			sub, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
			sub = context.WithValue(sub, sampleAttemptKey{}, sampleAttempt{Index: index + 1, Attempt: attempt})
			answer, err := probe.Sample(sub, account, t, ch.Prompt)
			cancel()
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			output := fingerprint.Output{Text: answer, ExpectedCount: ch.ExpectedCount}
			failure = ""
			if err != nil {
				failure = err.Error()
			} else {
				count, minimum := fingerprint.ValidateOutput(output)
				if count < minimum {
					failure = fmt.Sprintf("有效数字 %d 个，至少需要 %d 个", count, minimum)
				}
			}
			if failure == "" {
				samples = append(samples, output)
				break
			}
			if attempt == 3 {
				reason := fmt.Sprintf("第 %d/3 份样本重试 2 次后仍失败：%s；本轮不作指纹判定，稍后自动重试", index+1, failure)
				var configErr *ProbeConfigurationError
				if errors.As(err, &configErr) {
					return nil, "", configurationError(reason)
				}
				return &fingerprint.Analysis{Decision: "indeterminate", Expected: t.Expected, Used: len(samples), Diagnostic: reason, Version: detector.Version()}, reason, nil
			}
		}
	}
	analysis, err := detector.Analyze(samples, t.Expected)
	if err != nil {
		return nil, "指纹评分失败，稍后自动重试", nil
	}
	return &analysis, analysis.Diagnostic, nil
}

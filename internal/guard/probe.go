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
	"strings"
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
	// TransportTemplate is cloned for each sample; never a forwarding pool.
	TransportTemplate *http.Transport
}

// PreparedProbe separates local validation/identity lookup from quota reservation.
type PreparedProbe interface {
	Prepare(context.Context, Account, Target, string) (PreparedSample, error)
}
type PreparedSample interface {
	Sample(context.Context, func(SampleProgress)) (string, error)
}
type preparedRequest struct {
	request   *http.Request
	transport *http.Transport
}

func (p *Prober) Sample(ctx context.Context, a Account, t Target, prompt string) (string, error) {
	return p.SampleWithProgress(ctx, a, t, prompt, nil)
}
func (p *Prober) SampleWithProgress(ctx context.Context, a Account, t Target, prompt string, progress func(SampleProgress)) (string, error) {
	if progress != nil {
		progress(SampleProgress{Phase: "identity"})
	}
	prepared, err := p.Prepare(ctx, a, t, prompt)
	if err != nil {
		return "", err
	}
	return prepared.Sample(ctx, progress)
}

func (p *Prober) Prepare(ctx context.Context, a Account, t Target, prompt string) (PreparedSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endpoint := "https://chatgpt.com/backend-api/codex/responses"
	id := Identity{Headers: http.Header{}}
	switch a.Type {
	case "oauth":
		if p.Resolve == nil {
			return nil, configurationError("宿主不支持账号身份解析")
		}
		var err error
		id, err = p.Resolve(ctx, a.ID)
		if err != nil {
			return nil, configurationError("宿主无法解析账号出站身份")
		}
	case "apikey":
		var err error
		endpoint, err = apiResponsesURL(credential(a, "base_url"))
		if err != nil {
			return nil, configurationError(err.Error())
		}
		id.Token, id.Proxy = credential(a, "api_key"), a.OutboundProxy
	default:
		return nil, configurationError("账号类型不支持")
	}
	if strings.TrimSpace(id.Token) == "" {
		return nil, configurationError("账号没有可用出站凭据")
	}
	model := t.Model
	if a.Type == "apikey" {
		model = mappedModel(a, model)
	}
	if strings.TrimSpace(model) == "" {
		return nil, configurationError("请求模型不能为空")
	}
	payload := map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}, "instructions": "You are a helpful assistant.", "stream": true, "store": false}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, configurationError("无法编码探测请求")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, configurationError("无法创建探测请求")
	}
	req.Header = id.Headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if a.Type == "oauth" && req.Header.Get("OpenAI-Beta") == "" {
		req.Header.Set("OpenAI-Beta", "responses=experimental")
	}
	// Validate header bytes before quota is reserved; net/http otherwise fails locally.
	for k, values := range req.Header {
		if !validHeaderName(k) {
			return nil, configurationError("出站请求头名称无效")
		}
		for _, value := range values {
			if !validHeaderValue(value) {
				return nil, configurationError("出站请求头值无效")
			}
		}
	}
	transport, err := newTransport(id.Proxy)
	if err != nil {
		return nil, configurationError("代理地址无效")
	}
	// Probes never enter the production connection pool or reuse a previous sample.
	if p.TransportTemplate != nil {
		template := p.TransportTemplate.Clone()
		template.Proxy = transport.Proxy
		template.ResponseHeaderTimeout = 0
		transport = template
	}
	transport.DisableKeepAlives = true
	return &preparedRequest{request: req, transport: transport}, nil
}
func (p *preparedRequest) Sample(ctx context.Context, progress func(SampleProgress)) (string, error) {
	defer p.transport.CloseIdleConnections()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if progress != nil {
		progress(SampleProgress{Phase: "connecting"})
	}
	client := &http.Client{Transport: p.transport, CheckRedirect: NoRedirect}
	res, err := client.Do(p.request.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("上游连接失败或超时")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", upstreamResponseError(res)
	}
	if progress != nil {
		progress(SampleProgress{Phase: "receiving"})
	}
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
	unavailable := func(reason string) (*fingerprint.Analysis, string, error) {
		return &fingerprint.Analysis{Decision: "indeterminate", Expected: t.Expected, Used: len(samples), Diagnostic: reason, Version: detector.Version()}, reason, nil
	}
	for index, ch := range newFingerprintChallenges() {
		var failure string
		var previous error
		for attempt := 1; attempt <= 3; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			if attempt > 1 {
				delay, allowed := probeRetryDelay(previous, attempt)
				if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
					allowed = false
				}
				if !allowed {
					return unavailable(fmt.Sprintf("第 %d/3 份样本：%s；限流等待超过本轮可用时间，稍后重试", index+1, failure))
				}
				if observer, ok := probe.(retryObserver); ok {
					if err := observer.Retry(ctx, account, index+1, attempt, failure, delay); err != nil {
						return nil, "", err
					}
				} else if err := waitRetry(ctx, delay); err != nil {
					return nil, "", err
				}
			}
			sub, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
			sub = context.WithValue(sub, sampleAttemptKey{}, sampleAttempt{Index: index + 1, Attempt: attempt})
			var prepared PreparedSample
			var err error
			if preparer, ok := probe.(PreparedProbe); ok {
				prepared, err = preparer.Prepare(sub, account, t, ch.Prompt)
			}
			// All identity/endpoint/proxy/header validation precedes budget accounting.
			// Reservation remains conservative immediately before Do: an ambiguous
			// connection failure is never refunded and cannot cause a quota overrun.
			var answer string
			if err == nil {
				if err = sub.Err(); err == nil {
					if budgetErr := reserve(attempt > 1); budgetErr != nil {
						cancel()
						return nil, "", budgetErr
					}
				}
				if err == nil {
					if prepared != nil {
						answer, err = prepared.Sample(sub, nil)
					} else {
						answer, err = probe.Sample(sub, account, t, ch.Prompt)
					}
				}
			}
			cancel()
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			var configErr *ProbeConfigurationError
			if errors.As(err, &configErr) {
				return nil, "", err
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
			previous = err
			if attempt == 3 {
				return unavailable(fmt.Sprintf("第 %d/3 份样本重试 2 次后仍失败：%s；本轮不作指纹判定，稍后自动重试", index+1, failure))
			}
		}
	}
	analysis, err := detector.AnalyzeWithOptions(samples, t.Expected, fingerprint.AnalyzeOptions{FamilyMatch: c.FamilyMatch})
	if err != nil {
		return nil, "指纹评分失败，稍后自动重试", nil
	}
	return &analysis, analysis.Diagnostic, nil
}

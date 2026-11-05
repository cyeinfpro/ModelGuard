package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AdminHTTPError carries only safe diagnostics, never request headers or URLs.
type AdminHTTPError struct {
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

func (e *AdminHTTPError) Error() string { return e.Message }

type UpstreamHTTPError struct {
	StatusCode int
	RetryAfter time.Duration
	Code, Type string
}

func (e *UpstreamHTTPError) Error() string {
	s := fmt.Sprintf("上游 HTTP %d", e.StatusCode)
	if e.Code != "" {
		s += " · " + e.Code
	}
	if e.Type != "" && e.Type != e.Code {
		s += " · " + e.Type
	}
	return s
}

// ParseRetryAfter supports both delta-seconds and an HTTP date. Overflow and
// negative values are ignored; callers must not retry before a positive value.
func ParseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		// A long server cooldown ends the round; it is not shortened into a retry.
		if n > 86400 {
			n = 86400
		}
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(now) {
		d := at.Sub(now)
		if d > 24*time.Hour {
			return 24 * time.Hour
		}
		return d
	}
	return 0
}

func safeErrorToken(raw string) string {
	// Whitelist protocol identifiers, not merely their characters: providers can
	// echo an arbitrary API key into error.code/type. Unknown values stay private.
	switch strings.ToLower(raw) {
	case "invalid_api_key", "invalid_authentication", "authentication_error",
		"permission_denied", "insufficient_permissions", "invalid_request_error",
		"invalid_request", "model_not_found", "model_not_supported", "unsupported_model",
		"unsupported_value", "unsupported_parameter", "missing_required_parameter",
		"invalid_value", "invalid_parameter", "invalid_parameter_error",
		"context_length_exceeded", "rate_limit_exceeded", "rate_limit_error",
		"insufficient_quota", "quota_exceeded", "billing_hard_limit_reached",
		"billing_not_active", "account_deactivated", "access_terminated",
		"organization_deactivated", "server_error", "internal_server_error",
		"overloaded_error", "temporarily_unavailable", "service_unavailable",
		"timeout", "request_timeout", "not_found_error", "bad_request_error",
		"forbidden", "not_found", "unprocessable_entity", "invalid_json":
		return strings.ToLower(raw)
	default:
		return ""
	}
}

func upstreamResponseError(res *http.Response) error {
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&body)
	e := &UpstreamHTTPError{StatusCode: res.StatusCode, RetryAfter: ParseRetryAfter(res.Header.Get("Retry-After"), time.Now()), Code: safeErrorToken(body.Error.Code), Type: safeErrorToken(body.Error.Type)}
	code := e.StatusCode
	if code >= 300 && code < 500 && code != 408 && code != 409 && code != 425 && code != 429 {
		return configurationError(e.Error())
	}
	return e
}
func probeRetryDelay(err error, attempt int) (time.Duration, bool) {
	var permanent *ProbeConfigurationError
	if errors.As(err, &permanent) {
		return 0, false
	}
	base := 5 * time.Second
	if attempt >= 3 {
		base = 15 * time.Second
	}
	delay := time.Duration(float64(base) * (.8 + rand.Float64()*.4))
	var status *UpstreamHTTPError
	if errors.As(err, &status) && status.RetryAfter > delay {
		delay = status.RetryAfter
	}
	return delay, delay <= 60*time.Second
}

// AdminConnectionError preserves retry classification without exposing URLs or credentials.
type AdminConnectionError struct{ Cause error }

func (e *AdminConnectionError) Error() string { return "Sub2API 管理接口连接失败" }
func (e *AdminConnectionError) Unwrap() error { return e.Cause }

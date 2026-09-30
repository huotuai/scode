package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// UserAgent identifies scode on provider HTTP requests; the version
// tracks the app release (desktop/package.json).
const UserAgent = "SCode/0.1.0"

// RetryableStatus marks an HTTP response that warrants a retry.
type RetryableStatus struct {
	Code       int
	RetryAfter string
}

func (e *RetryableStatus) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// TransportConfig carries pi's settings.retry.provider knobs:
// per-request timeout, transport retry attempts, and the backoff cap.
type TransportConfig struct {
	TimeoutMs       int // 0 = DefaultTransport().TimeoutMs
	MaxAttempts     int // total attempts (retries + 1); 0 = default
	MaxRetryDelayMs int // backoff cap; 0 = default 60000 (pi)
}

// DefaultTransport is the built-in transport policy (pi's defaults
// where they exist: the delay cap is 60s).
func DefaultTransport() TransportConfig {
	return TransportConfig{TimeoutMs: 10 * 60_000, MaxAttempts: 4, MaxRetryDelayMs: 60_000}
}

// Timeout resolves the per-request timeout with defaults applied.
func (c TransportConfig) Timeout() time.Duration {
	return time.Duration(c.withDefaults().TimeoutMs) * time.Millisecond
}

func (c TransportConfig) withDefaults() TransportConfig {
	d := DefaultTransport()
	if c.TimeoutMs > 0 {
		d.TimeoutMs = c.TimeoutMs
	}
	if c.MaxAttempts > 0 {
		d.MaxAttempts = c.MaxAttempts
	}
	if c.MaxRetryDelayMs > 0 {
		d.MaxRetryDelayMs = c.MaxRetryDelayMs
	}
	return d
}

// PostStreamWithRetry sends a JSON POST expected to return an SSE stream,
// retrying transient failures (network errors, 408/429/5xx) with
// exponential backoff and Retry-After support, delays capped by the
// transport config (pi's maxRetryDelayMs). A non-retryable non-200
// response is returned to the caller to surface as a single error.
// Retries stop once a 200 body starts streaming: mid-stream failures are
// terminal events, and retrying them is the agent loop's policy decision.
func PostStreamWithRetry(ctx context.Context, client *http.Client, url string, headers map[string][]string, body []byte, cfg TransportConfig) (*http.Response, error) {
	cfg = cfg.withDefaults()
	if client == nil {
		client = http.DefaultClient
	}
	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt, lastErr, cfg.MaxRetryDelayMs); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", UserAgent)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusTooManyRequests,
			resp.StatusCode == http.StatusRequestTimeout,
			resp.StatusCode >= 500:
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			lastErr = &RetryableStatus{Code: resp.StatusCode, RetryAfter: resp.Header.Get("retry-after")}
			continue
		default:
			return resp, nil
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", cfg.MaxAttempts, lastErr)
}

func sleepBackoff(ctx context.Context, attempt int, cause error, capMs int) error {
	d := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s...
	d += time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
	if rs, ok := cause.(*RetryableStatus); ok && rs.RetryAfter != "" {
		if secs, err := strconv.Atoi(rs.RetryAfter); err == nil {
			d = time.Duration(secs) * time.Second
		}
	}
	if max := time.Duration(capMs) * time.Millisecond; d > max {
		d = max // pi's maxRetryDelayMs cap
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

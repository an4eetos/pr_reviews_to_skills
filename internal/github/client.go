// Package github fetches pull requests with their review discussions through
// the GitHub GraphQL API, paginating nested connections and staying inside
// the primary and secondary rate limits.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TransientError is returned when GitHub keeps timing out (502/503/504) on a
// query; the caller can retry with a smaller page.
type TransientError struct {
	Status int
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("github: transient HTTP %d after retries", e.Status)
}

type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
	Logf     func(format string, args ...any)

	// Overridable in tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

func NewClient(endpoint, token string, logf func(string, ...any)) *Client {
	return &Client{
		Endpoint: endpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 90 * time.Second},
		Logf:     logf,
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

const (
	maxAttempts       = 8
	maxTransientTries = 3
)

// Query runs a GraphQL query, decodes data into out, and throttles against
// the rateLimit block every query in this package selects.
func (c *Client) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	transient := 0
	backoff := 30 * time.Second
	for attempt := 1; ; attempt++ {
		if attempt > maxAttempts {
			return fmt.Errorf("github: giving up after %d attempts", maxAttempts)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "prrules")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Client-side timeouts on heavy pages behave like 502s.
			transient++
			if transient >= maxTransientTries {
				return &TransientError{Status: 0}
			}
			c.logf("github: request error (%v), retrying", err)
			if err := c.sleep(ctx, time.Duration(transient)*5*time.Second); err != nil {
				return err
			}
			continue
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			return errors.New("github: 401 unauthorized - check GITHUB_TOKEN")
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			wait := c.rateLimitWait(resp.Header, backoff)
			if wait == backoff {
				backoff = min(backoff*2, 10*time.Minute)
			}
			c.logf("github: rate limited (HTTP %d), sleeping %s", resp.StatusCode, wait.Round(time.Second))
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
			transient++
			if transient >= maxTransientTries {
				return &TransientError{Status: resp.StatusCode}
			}
			c.logf("github: HTTP %d, retrying", resp.StatusCode)
			if err := c.sleep(ctx, time.Duration(transient)*5*time.Second); err != nil {
				return err
			}
			continue
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("github: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
		}

		var gr gqlResponse
		if err := json.Unmarshal(raw, &gr); err != nil {
			return fmt.Errorf("github: decoding response: %w", err)
		}
		var rl struct {
			RateLimit *rateLimit `json:"rateLimit"`
		}
		if len(gr.Data) > 0 && string(gr.Data) != "null" {
			_ = json.Unmarshal(gr.Data, &rl)
		}
		if len(gr.Errors) > 0 {
			if isRateLimited(gr.Errors) {
				wait := backoff
				if rl.RateLimit != nil {
					wait = max(rl.RateLimit.ResetAt.Sub(c.now())+5*time.Second, 5*time.Second)
				}
				c.logf("github: GraphQL rate limit hit, sleeping %s", wait.Round(time.Second))
				if err := c.sleep(ctx, wait); err != nil {
					return err
				}
				continue
			}
			msgs := make([]string, len(gr.Errors))
			for i, e := range gr.Errors {
				msgs[i] = e.Message
			}
			return fmt.Errorf("github: %s", strings.Join(msgs, "; "))
		}
		if err := json.Unmarshal(gr.Data, out); err != nil {
			return fmt.Errorf("github: decoding data: %w", err)
		}
		if rl.RateLimit != nil {
			if err := c.throttle(ctx, *rl.RateLimit); err != nil {
				return err
			}
		}
		return nil
	}
}

// throttle sleeps until the window resets when the remaining points would not
// cover a few more queries of the same cost.
func (c *Client) throttle(ctx context.Context, rl rateLimit) error {
	if rl.Remaining >= max(rl.Cost*3, 50) {
		return nil
	}
	wait := rl.ResetAt.Sub(c.now()) + 5*time.Second
	if wait <= 0 {
		return nil
	}
	c.logf("github: %d points left (query cost %d), sleeping %s until reset", rl.Remaining, rl.Cost, wait.Round(time.Second))
	return c.sleep(ctx, wait)
}

// rateLimitWait reads Retry-After (secondary limits) or x-ratelimit-reset
// (primary limit); otherwise it falls back to the exponential backoff.
func (c *Client) rateLimitWait(h http.Header, fallback time.Duration) time.Duration {
	if s := h.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			return time.Duration(secs)*time.Second + time.Second
		}
	}
	if h.Get("X-Ratelimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			if d := time.Unix(reset, 0).Sub(c.now()) + 5*time.Second; d > 0 {
				return d
			}
		}
	}
	return fallback
}

func isRateLimited(errs []gqlError) bool {
	for _, e := range errs {
		if e.Type == "RATE_LIMITED" || strings.Contains(strings.ToLower(e.Message), "rate limit") {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Package cubepath is a small client for the parts of the CubePath customer API
// the exporter reads: the GraphQL endpoint.
package cubepath

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxResponseBytes caps what the client reads from any response.
const MaxResponseBytes = 16 << 20

// DefaultTimeout is the per-request timeout when Options.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// API names used as the first label of the request counter.
const (
	APIGraphQL = "graphql"
)

// Request results used as the second label of the request counter.
const (
	ResultOK          = "ok"
	ResultHTTPError   = "http_error"
	ResultRateLimited = "rate_limited"
	ResultNetwork     = "network_error"
	ResultDecode      = "decode_error"
)

// Options configures a Client.
type Options struct {
	BaseURL   string
	Token     Secret
	UserAgent string
	Timeout   time.Duration
	// Transport is optional (tests). TLS verification is never disabled by this package.
	Transport http.RoundTripper
	// Observe is called once per HTTP request with the API and the result.
	Observe func(api, result string)
	// Now is optional (tests).
	Now func() time.Time
}

// Client talks to the CubePath API. It is safe for concurrent use.
type Client struct {
	base    *url.URL
	token   Secret
	ua      string
	hc      *http.Client
	observe func(api, result string)
	now     func() time.Time
}

// ValidateBaseURL checks an API URL: https only, except http for localhost and 127.0.0.1.
func ValidateBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid API URL: %w", err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("invalid API URL: missing host")
	}
	if u.User != nil {
		return nil, errors.New("invalid API URL: credentials in the URL are not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid API URL: query and fragment are not allowed")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
			return nil, errors.New("invalid API URL: http is only allowed for localhost and 127.0.0.1")
		}
	default:
		return nil, errors.New("invalid API URL: scheme must be https")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// New builds a Client.
func New(o Options) (*Client, error) {
	base, err := ValidateBaseURL(o.BaseURL)
	if err != nil {
		return nil, err
	}
	if o.Token == "" {
		return nil, errors.New("an API token is required")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rt := o.Transport
	if rt == nil {
		rt = http.DefaultTransport.(*http.Transport).Clone()
	}
	c := &Client{
		base:    base,
		token:   o.Token,
		ua:      o.UserAgent,
		observe: o.Observe,
		now:     o.Now,
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.hc = &http.Client{
		Timeout:   timeout,
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Never follow a redirect to another host or to a weaker scheme.
			if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				return errors.New("refusing cross-origin redirect")
			}
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	return c, nil
}

func (c *Client) record(api, result string) {
	if c.observe != nil {
		c.observe(api, result)
	}
}

// do performs one request and decodes a JSON body into out (when out is not nil).
func (c *Client) do(ctx context.Context, api, method, path string, body []byte, out any) error {
	u := *c.base
	u.Path = c.base.Path + path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	req.Header.Set("Accept", "application/json")
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.record(api, ResultNetwork)
		// url.Error holds the URL and the cause, never the headers.
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		c.record(api, ResultRateLimited)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return &RateLimitError{RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), c.now())}
	case resp.StatusCode == http.StatusUnauthorized:
		c.record(api, ResultHTTPError)
		return ErrUnauthenticated
	case resp.StatusCode != http.StatusOK:
		c.record(api, ResultHTTPError)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return &HTTPError{Status: resp.StatusCode}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		c.record(api, ResultNetwork)
		return err
	}
	if len(data) > MaxResponseBytes {
		c.record(api, ResultDecode)
		return ErrResponseTooLarge
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.record(api, ResultDecode)
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	c.record(api, ResultOK)
	return nil
}

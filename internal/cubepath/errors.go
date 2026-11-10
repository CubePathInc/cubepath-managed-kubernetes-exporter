package cubepath

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GraphQL error codes documented by the CubePath API.
const (
	CodeUnauthenticated   = "UNAUTHENTICATED"
	CodeForbidden         = "FORBIDDEN"
	CodeNotFound          = "NOT_FOUND"
	CodeBadUserInput      = "BAD_USER_INPUT"
	CodeMetricsUnavail    = "METRICS_UNAVAILABLE"
	CodeQueryTooComplex   = "QUERY_TOO_COMPLEX"
	CodeInternalError     = "INTERNAL_ERROR"
	CodeRateLimited       = "RATE_LIMITED"
	CodeNetwork           = "NETWORK"
	CodeUnknown           = "UNKNOWN"
	maxRetryAfter         = 10 * time.Minute
	defaultRetryAfterWait = 60 * time.Second
)

var knownCodes = map[string]bool{
	CodeUnauthenticated: true, CodeForbidden: true, CodeNotFound: true, CodeBadUserInput: true,
	CodeMetricsUnavail: true, CodeQueryTooComplex: true, CodeInternalError: true,
	CodeRateLimited: true, CodeNetwork: true, CodeUnknown: true,
}

// ErrUnauthenticated means the API did not accept the token.
var ErrUnauthenticated = errors.New("cubepath: credential rejected (UNAUTHENTICATED)")

// ErrResponseTooLarge means a response exceeded MaxResponseBytes.
var ErrResponseTooLarge = errors.New("cubepath: response too large")

// RateLimitError is an HTTP 429 from the API.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("cubepath: rate limited (retry after %s)", e.RetryAfter)
}

// HTTPError is an unexpected HTTP status (never carries the response body).
type HTTPError struct {
	Status int
}

func (e *HTTPError) Error() string {
	return "cubepath: unexpected HTTP status " + strconv.Itoa(e.Status)
}

// QueryError is a GraphQL failure that makes the whole operation useless.
type QueryError struct {
	Code string
}

func (e *QueryError) Error() string { return "cubepath: GraphQL error " + e.Code }

// ParseRetryAfter reads a Retry-After header (seconds or HTTP date), bounded to a sane range.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultRetryAfterWait
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := time.Parse(time.RFC1123, v); err == nil {
		d = t.Sub(now)
	} else {
		return defaultRetryAfterWait
	}
	if d < time.Second {
		d = time.Second
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

// NormalizeCode maps any API code to a bounded set, so it is safe as a label value.
func NormalizeCode(code string) string {
	if code == "" {
		return CodeUnknown
	}
	if knownCodes[code] {
		return code
	}
	return CodeUnknown
}

// ErrorCode classifies a transport or HTTP level error for the api_errors_total metric.
func ErrorCode(err error) string {
	var rl *RateLimitError
	var he *HTTPError
	var qe *QueryError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnauthenticated):
		return CodeUnauthenticated
	case errors.As(err, &rl):
		return CodeRateLimited
	case errors.As(err, &qe):
		return NormalizeCode(qe.Code)
	case errors.As(err, &he):
		return httpCode(he.Status)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeNetwork
	default:
		return CodeNetwork
	}
}

func httpCode(status int) string {
	switch {
	case status == 403 || status == 404 || status == 400:
		return "HTTP_" + strconv.Itoa(status)
	case status >= 500:
		return "HTTP_5XX"
	default:
		return "HTTP_OTHER"
	}
}

// Is lets errors.Is(err, ErrUnauthenticated) match a GraphQL UNAUTHENTICATED answer.
func (e *QueryError) Is(target error) bool {
	return target == ErrUnauthenticated && e.Code == CodeUnauthenticated
}

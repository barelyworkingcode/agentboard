package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

// Deadline is the whole budget of one client invocation: gathering and POST.
const Deadline = 250 * time.Millisecond

const maxResponse = 64 << 10

// SoftError means the post was not recorded but the caller should still exit
// 0: the server was unreachable, timed out, refused ingest (403) or failed (5xx).
type SoftError struct{ Reason string }

func (e *SoftError) Error() string { return "not recorded: " + e.Reason }

// RejectedError means the server rejected the request itself (4xx other than 403).
type RejectedError struct {
	Status  int
	Message string
}

func (e *RejectedError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Status) }

// httpClient ignores proxy variables, because a proxy would change the source
// address the ingest gate checks, and keeps no idle connections.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:             nil,
		DialContext:       (&net.Dialer{}).DialContext,
		DisableKeepAlives: true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Post sends in as JSON to baseURL+path and decodes a 2xx body into out
// (nil to discard). It returns nil, *SoftError or *RejectedError. The
// deadline comes from ctx.
func Post(ctx context.Context, baseURL, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return &SoftError{Reason: fmt.Sprintf("encode request: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return &SoftError{Reason: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return &SoftError{Reason: transportReason(ctx, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return &SoftError{Reason: transportReason(ctx, err)}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return &SoftError{Reason: fmt.Sprintf("decode response: %v", err)}
		}
		return nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500:
		return &SoftError{Reason: fmt.Sprintf("%s (%d)", serverMessage(raw, resp.StatusCode), resp.StatusCode)}
	case resp.StatusCode >= 400:
		return &RejectedError{Status: resp.StatusCode, Message: serverMessage(raw, resp.StatusCode)}
	default:
		return &SoftError{Reason: fmt.Sprintf("unexpected status %d", resp.StatusCode)}
	}
}

func serverMessage(raw []byte, status int) string {
	var e wire.ErrorResp
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return http.StatusText(status)
}

func transportReason(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// Report writes the one-line stderr message for err and returns the exit
// code: 0 for nil or *SoftError, 1 for *RejectedError, 2 otherwise.
func Report(stderr io.Writer, err error) int {
	var soft *SoftError
	var rej *RejectedError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &soft):
		fmt.Fprintf(stderr, "agentboard: %s\n", soft.Error())
		return 0
	case errors.As(err, &rej):
		fmt.Fprintf(stderr, "agentboard: %s\n", rej.Error())
		return 1
	default:
		fmt.Fprintf(stderr, "agentboard: %v; see agentboard help\n", err)
		return 2
	}
}

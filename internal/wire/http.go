package wire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const userAgent = "gitq/0.1"

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do sends a request and maps error statuses. On success the caller owns
// resp.Body and must close it via closeBody.
func (c *Client) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c.Auth != nil {
		if err := c.Auth(req); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAuth, err)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	closeBody(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && isRateLimited(resp):
		return nil, fmt.Errorf("%w: %s %s: %s", ErrRateLimited, method, req.URL.Redacted(), resp.Status)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s %s: %s", ErrAuth, method, req.URL.Redacted(), resp.Status)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, method, req.URL.Redacted())
	}
	return nil, fmt.Errorf("wire: %s %s: %s: %s", method, req.URL.Redacted(), resp.Status,
		strings.TrimSpace(string(snippet)))
}

func isRateLimited(resp *http.Response) bool {
	h := resp.Header
	return h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "" ||
		h.Get("RateLimit-Remaining") == "0"
}

// closeBody drains (bounded) and closes so the connection can be reused.
func closeBody(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
	resp.Body.Close()
}

func uploadPackHeaders() map[string]string {
	return map[string]string{
		"Git-Protocol": "version=2",
		"Content-Type": "application/x-git-upload-pack-request",
		"Accept":       "application/x-git-upload-pack-result",
	}
}

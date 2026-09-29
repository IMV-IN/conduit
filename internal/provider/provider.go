// Package provider speaks the OpenAI-compatible chat dialect to upstreams.
package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Class classifies upstream failures for the executor.
type Class int

const (
	ClassOK Class = iota
	ClassRetryable
	ClassContextExceeded
	ClassAuth
	ClassClient
	ClassRefusal
)

// Error carries a classified upstream error.
type Error struct {
	Class      Class
	HTTPStatus int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("provider error %d", e.HTTPStatus)
}

// IsRetryable reports whether the executor should fail over.
func IsRetryable(err error) bool {
	if pe, ok := err.(*Error); ok {
		return pe.Class == ClassRetryable
	}
	return false
}

func ClassifyStatus(status int, body []byte) *Error {
	s := string(body)
	switch {
	case status == 429 || (status >= 500 && status <= 599) || status == 503:
		return &Error{Class: ClassRetryable, HTTPStatus: status, Err: fmt.Errorf("retryable %d: %s", status, trunc(s))}
	case status == 401 || status == 403:
		return &Error{Class: ClassAuth, HTTPStatus: status, Err: fmt.Errorf("auth %d", status)}
	case status == 400 && contains(s, "context_length_exceeded"):
		return &Error{Class: ClassContextExceeded, HTTPStatus: status, Err: fmt.Errorf("context exceeded")}
	case status == 400 && contains(s, "content_filter"):
		return &Error{Class: ClassRefusal, HTTPStatus: status, Err: fmt.Errorf("refusal")}
	case status >= 400:
		return &Error{Class: ClassClient, HTTPStatus: status, Err: fmt.Errorf("client %d: %s", status, trunc(s))}
	}
	return nil
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func trunc(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// Client POSTs chat bodies to an upstream base URL.
type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 120 * time.Second,
	}}
}

// DoChat sends rawBody (with model already spliced to upstream) and returns the response.
func (c *Client) DoChat(ctx context.Context, baseURL, apiKey string, rawBody []byte) (*http.Response, error) {
	url := baseURL
	if len(url) > 0 && url[len(url)-1] == '/' {
		url += "chat/completions"
	} else {
		url += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Class: ClassRetryable, Err: err}
	}
	return resp, nil
}

// DrainAndClassify reads a non-streaming error body and classifies it.
func DrainAndClassify(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if perr := ClassifyStatus(resp.StatusCode, b); perr != nil {
		return perr
	}
	return nil
}

// Package notify sends ntfy pushes and decides which are due.
package notify

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sender delivers one push. Tests substitute a fake.
type Sender interface {
	Send(title, body string) error
}

// Client posts to an ntfy topic URL (https://ntfy.sh/<topic> or settings.ntfy_url).
type Client struct {
	url  string
	http *http.Client
}

// NewClient returns a Client with the given request timeout.
func NewClient(url string, timeout time.Duration) *Client {
	return &Client{url: url, http: &http.Client{Timeout: timeout}}
}

// Send POSTs body with a Title header; any non-2xx status is an error.
func (c *Client) Send(title, body string) error {
	req, err := http.NewRequest(http.MethodPost, c.url, strings.NewReader(body))
	if err != nil {
		return withoutURL(err)
	}
	req.Header.Set("Title", title)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ntfy: %s", resp.Status)
	}
	return nil
}

// withoutURL drops the request URL from a net/url error: its path is the ntfy topic, which is the password,
// and these errors end up in brain.err.log.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("ntfy: %s: %w", ue.Op, ue.Err)
	}
	return err
}

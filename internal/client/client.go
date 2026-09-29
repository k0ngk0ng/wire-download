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
	"path/filepath"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/daemon"
	"github.com/k0ngk0ng/wire-download/internal/video"
)

type Client struct{ http *http.Client }

type HTTPError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("daemon API %s %s: HTTP %d %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
}

type Status struct {
	Version string            `json:"version"`
	Jobs    []daemon.Job      `json:"jobs"`
	Engines map[string]string `json:"engines"`
}

func New(dir string) *Client {
	return &Client{http: &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "daemon.sock"))
	}}}}
}
func (c *Client) Call(ctx context.Context, method, path string, body, result any) error {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unavailable (use wirectl download daemon start): %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&e); err != nil {
			e.Error = ""
		}
		return &HTTPError{StatusCode: res.StatusCode, Method: method, Path: path, Message: e.Error}
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result)
}
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.Call(ctx, "GET", "/v1/status", nil, &s)
	return s, err
}
func (c *Client) Add(ctx context.Context, source string) (daemon.Job, error) {
	var j daemon.Job
	err := c.Call(ctx, "POST", "/v1/jobs", map[string]string{"source": source}, &j)
	return j, err
}
func (c *Client) Action(ctx context.Context, id, action string) error {
	return c.Call(ctx, "POST", "/v1/jobs/"+id+"/"+action, nil, nil)
}

func (c *Client) Submit(ctx context.Context, source string) (daemon.Submission, error) {
	_, _, _, handled, err := video.Source(source)
	if err != nil {
		return daemon.Submission{}, err
	}
	// Keep ordinary downloads compatible with daemons predating video support.
	// Never retry a video as an ordinary URL: that would download HTML.
	if !handled {
		job, err := c.Add(ctx, source)
		if err != nil {
			return daemon.Submission{}, err
		}
		return daemon.Submission{Jobs: []daemon.Job{job}}, nil
	}
	var result daemon.Submission
	httpClient := *c.http
	httpClient.Timeout = 4 * time.Minute
	extended := &Client{http: &httpClient}
	err = extended.Call(ctx, "POST", "/v1/submissions", map[string]string{"source": source}, &result)
	var response *HTTPError
	if errors.As(err, &response) && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed) {
		return result, fmt.Errorf("running daemon does not support video submissions; upgrade wire-download and run 'wirectl download daemon restart' (use the same --data-dir if set): %w", err)
	}
	return result, err
}

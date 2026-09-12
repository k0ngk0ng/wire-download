package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/daemon"
)

type Client struct{ http *http.Client }
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
			return fmt.Errorf("daemon HTTP %d", res.StatusCode)
		}
		return fmt.Errorf("%s", e.Error)
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

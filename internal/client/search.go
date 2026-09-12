package client

import (
	"context"
	"net/url"

	"github.com/k0ngk0ng/wire-download/internal/daemon"
	"github.com/k0ngk0ng/wire-download/internal/search"
)

func (c *Client) Search(ctx context.Context, req search.Request) (search.Snapshot, error) {
	var s search.Snapshot
	err := c.Call(ctx, "POST", "/v1/searches", req, &s)
	return s, err
}
func (c *Client) SearchResults(ctx context.Context, id string) (search.Snapshot, error) {
	var s search.Snapshot
	err := c.Call(ctx, "GET", "/v1/searches/"+url.PathEscape(id), nil, &s)
	return s, err
}
func (c *Client) CancelSearch(ctx context.Context, id string) error {
	return c.Call(ctx, "DELETE", "/v1/searches/"+url.PathEscape(id), nil, nil)
}
func (c *Client) DownloadSearchResult(ctx context.Context, id, result string) (daemon.Job, error) {
	var j daemon.Job
	err := c.Call(ctx, "POST", "/v1/searches/"+url.PathEscape(id)+"/results/"+url.PathEscape(result)+"/download", nil, &j)
	return j, err
}
func (c *Client) SearchSources(ctx context.Context) ([]search.SourceInfo, error) {
	var s []search.SourceInfo
	err := c.Call(ctx, "GET", "/v1/search-sources", nil, &s)
	return s, err
}
func (c *Client) SetSearchSource(ctx context.Context, s search.SourceConfig) error {
	return c.Call(ctx, "PUT", "/v1/search-sources/"+url.PathEscape(s.ID), s, nil)
}
func (c *Client) EnableSearchSource(ctx context.Context, id string, enabled bool) error {
	return c.Call(ctx, "PATCH", "/v1/search-sources/"+url.PathEscape(id), map[string]bool{"enabled": enabled}, nil)
}
func (c *Client) RemoveSearchSource(ctx context.Context, id string) error {
	return c.Call(ctx, "DELETE", "/v1/search-sources/"+url.PathEscape(id), nil, nil)
}

package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Aria2 struct {
	URL, Secret string
	Client      *http.Client
}

func NewAria2(port int, secret string) *Aria2 {
	return &Aria2{URL: fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port), Secret: secret, Client: &http.Client{Timeout: 15 * time.Second}}
}
func (a *Aria2) Call(ctx context.Context, method string, params []any, result any) error {
	params = append([]any{"token:" + a.Secret}, params...)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "wirectl", "method": "aria2." + method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return fmt.Errorf("aria2 unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("aria2 HTTP status %d", resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("aria2 response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("aria2 error %d: %s", envelope.Error.Code, strings.ReplaceAll(envelope.Error.Message, a.Secret, "[redacted]"))
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, result)
}
func (a *Aria2) Add(ctx context.Context, source string) (string, error) {
	return a.AddWithID(ctx, source, "")
}
func (a *Aria2) AddWithID(ctx context.Context, source, idHint string) (string, error) {
	options := map[string]string{}
	if idHint != "" {
		options["gid"] = idHint
	}
	var id string
	if strings.HasSuffix(strings.ToLower(source), ".torrent") && !strings.Contains(source, "://") {
		f, err := os.Open(source)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
		if err != nil {
			return "", err
		}
		if len(b) > 16<<20 {
			return "", fmt.Errorf("torrent exceeds 16 MiB")
		}
		err = a.Call(ctx, "addTorrent", []any{base64.StdEncoding.EncodeToString(b), []string{}, options}, &id)
		return id, err
	}
	err := a.Call(ctx, "addUri", []any{[]string{source}, options}, &id)
	return id, err
}

type ariaStatus struct {
	GID          string   `json:"gid"`
	Status       string   `json:"status"`
	Total        string   `json:"totalLength"`
	Completed    string   `json:"completedLength"`
	Down         string   `json:"downloadSpeed"`
	Up           string   `json:"uploadSpeed"`
	ErrorMessage string   `json:"errorMessage"`
	FollowedBy   []string `json:"followedBy"`
	Files        []struct {
		Path string `json:"path"`
		URIs []struct {
			URI string `json:"uri"`
		} `json:"uris"`
	} `json:"files"`
	BitTorrent struct {
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
}

func (s ariaStatus) item() Item {
	n := func(v string) int64 { x, _ := strconv.ParseInt(v, 10, 64); return x }
	i := Item{ID: s.GID, Name: s.BitTorrent.Info.Name, Status: s.Status, Total: n(s.Total), Completed: n(s.Completed), DownloadRate: n(s.Down), UploadRate: n(s.Up), Error: s.ErrorMessage}
	if i.Name == "" && len(s.Files) > 0 {
		i.Name = filepath.Base(s.Files[0].Path)
		if i.Name == "." && len(s.Files[0].URIs) > 0 {
			i.Name = s.Files[0].URIs[0].URI
		}
	}
	if i.Total > 0 {
		i.Progress = float64(i.Completed) / float64(i.Total) * 100
		if i.Completed == i.Total && i.Status == "active" {
			i.Status = "seeding"
		}
	}
	if len(s.FollowedBy) > 0 {
		i.Status = "metadata"
		i.FollowedBy = s.FollowedBy
	}
	return i
}
func (a *Aria2) List(ctx context.Context) ([]Item, error) {
	items := []Item{}
	var active []ariaStatus
	if err := a.Call(ctx, "tellActive", nil, &active); err != nil {
		return nil, err
	}
	for _, s := range active {
		items = append(items, s.item())
	}
	for _, method := range []string{"tellWaiting", "tellStopped"} {
		for offset := 0; ; offset += 1000 {
			var page []ariaStatus
			if err := a.Call(ctx, method, []any{offset, 1000}, &page); err != nil {
				return nil, err
			}
			for _, s := range page {
				items = append(items, s.item())
			}
			if len(page) < 1000 {
				break
			}
		}
	}
	return items, nil
}
func (a *Aria2) Pause(ctx context.Context, id string) error {
	return a.Call(ctx, "pause", []any{id}, nil)
}
func (a *Aria2) Resume(ctx context.Context, id string) error {
	return a.Call(ctx, "unpause", []any{id}, nil)
}
func (a *Aria2) Remove(ctx context.Context, id string) error {
	return a.Call(ctx, "remove", []any{id}, nil)
}
func (a *Aria2) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.Call(ctx, "saveSession", nil, nil)
}

func (a *Aria2) SaveSession(ctx context.Context) error { return a.Call(ctx, "saveSession", nil, nil) }

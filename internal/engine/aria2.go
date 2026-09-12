package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Aria2 struct {
	URL, Secret string
	Client      *http.Client

	// forceSaved tracks GIDs for which the per-download force-save option has
	// already been applied. aria2 is polled frequently, so doing this RPC on
	// every List call would add needless traffic.
	forceSaveMu sync.Mutex
	forceSaved  map[string]struct{}
}

func NewAria2(port int, secret string) *Aria2 {
	return &Aria2{URL: fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port), Secret: secret, Client: &http.Client{Timeout: 15 * time.Second}, forceSaved: map[string]struct{}{}}
}

// aria2RPCError preserves the JSON-RPC error code while keeping the public
// error text compatible with the old string errors. The typed error lets
// cleanup distinguish RPC failures from transport failures.
type aria2RPCError struct {
	code    int
	message string
}

func (e *aria2RPCError) Error() string {
	return fmt.Sprintf("aria2 error %d: %s", e.code, e.message)
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
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&envelope); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("aria2 HTTP status %d", resp.StatusCode)
		}
		return fmt.Errorf("aria2 response: %w", err)
	}
	if envelope.Error != nil {
		return &aria2RPCError{code: envelope.Error.Code, message: strings.ReplaceAll(envelope.Error.Message, a.Secret, "[redacted]")}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("aria2 HTTP status %d", resp.StatusCode)
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
	// The daemon keeps the global aria2 setting at force-save=false so that
	// ordinary HTTP results do not come back after a restart. A local torrent
	// or magnet is a live BitTorrent job, however, and must opt in to session
	// persistence so that seeding can resume.
	forceSave := strings.HasPrefix(strings.ToLower(source), "magnet:") ||
		(strings.HasSuffix(strings.ToLower(source), ".torrent") && !strings.Contains(source, "://"))
	if forceSave {
		options["force-save"] = "true"
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
		if err == nil && forceSave {
			a.rememberForceSaved(id)
		}
		return id, err
	}
	err := a.Call(ctx, "addUri", []any{[]string{source}, options}, &id)
	if err == nil && forceSave {
		a.rememberForceSaved(id)
	}
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

func (a *Aria2) rememberForceSaved(id string) {
	if id == "" {
		return
	}
	a.forceSaveMu.Lock()
	if a.forceSaved == nil {
		a.forceSaved = map[string]struct{}{}
	}
	a.forceSaved[id] = struct{}{}
	a.forceSaveMu.Unlock()
}

func (a *Aria2) forgetForceSaved(id string) {
	if id == "" {
		return
	}
	a.forceSaveMu.Lock()
	delete(a.forceSaved, id)
	a.forceSaveMu.Unlock()
}

func (a *Aria2) forceSavedFor(id string) bool {
	a.forceSaveMu.Lock()
	_, ok := a.forceSaved[id]
	a.forceSaveMu.Unlock()
	return ok
}

func (a *Aria2) ensureForceSaved(ctx context.Context, id string) error {
	if id == "" || a.forceSavedFor(id) {
		return nil
	}
	if err := a.Call(ctx, "changeOption", []any{id, map[string]string{"force-save": "true"}}, nil); err != nil {
		// A task can disappear between tellActive/tellStopped and
		// changeOption. It is already gone, so this is harmless to List;
		// preserve every other RPC error for the caller.
		if isAria2NotFound(err) {
			return nil
		}
		if isAria2CannotChangeOption(err) {
			// A task can finish between tellActive/tellStopped and this RPC.
			// Confirm that race before treating the change as unnecessary; an
			// active or waiting task must still surface the original failure.
			var status ariaStatus
			statusErr := a.Call(ctx, "tellStatus", []any{id}, &status)
			if isAria2NotFound(statusErr) || (statusErr == nil && isTerminalAriaStatus(status.Status)) {
				return nil
			}
		}
		return err
	}
	a.rememberForceSaved(id)
	return nil
}

func isBitTorrentStatus(s ariaStatus) bool {
	if s.BitTorrent.Info.Name == "" {
		return false
	}
	switch s.Status {
	case "active", "waiting", "paused", "seeding":
		return true
	default:
		return false
	}
}

func (a *Aria2) List(ctx context.Context) ([]Item, error) {
	items := []Item{}
	appendStatus := func(s ariaStatus) error {
		if isBitTorrentStatus(s) {
			if err := a.ensureForceSaved(ctx, s.GID); err != nil {
				return err
			}
		}
		items = append(items, s.item())
		return nil
	}
	var active []ariaStatus
	if err := a.Call(ctx, "tellActive", nil, &active); err != nil {
		return nil, err
	}
	for _, s := range active {
		if err := appendStatus(s); err != nil {
			return nil, err
		}
	}
	for _, method := range []string{"tellWaiting", "tellStopped"} {
		for offset := 0; ; offset += 1000 {
			var page []ariaStatus
			if err := a.Call(ctx, method, []any{offset, 1000}, &page); err != nil {
				return nil, err
			}
			for _, s := range page {
				if err := appendStatus(s); err != nil {
					return nil, err
				}
			}
			if len(page) < 1000 {
				break
			}
		}
	}
	return items, nil
}
func (a *Aria2) Pause(ctx context.Context, id string) error {
	if err := a.Call(ctx, "pause", []any{id}, nil); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status ariaStatus
		if err := a.Call(waitCtx, "tellStatus", []any{id}, &status); err != nil {
			if waitCtx.Err() != nil {
				return waitCtx.Err()
			}
			return err
		}
		switch status.Status {
		case "paused":
			return nil
		case "complete", "error", "removed":
			if status.ErrorMessage != "" {
				return fmt.Errorf("aria2 pause %s: task reached %s: %s", id, status.Status, status.ErrorMessage)
			}
			return fmt.Errorf("aria2 pause %s: task reached %s", id, status.Status)
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}
func (a *Aria2) Resume(ctx context.Context, id string) error {
	return a.Call(ctx, "unpause", []any{id}, nil)
}

func isTerminalAriaStatus(status string) bool {
	switch status {
	case "complete", "error", "removed":
		return true
	default:
		return false
	}
}

func isAria2NotFound(err error) bool {
	var rpcErr *aria2RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.message)
	// aria2 reports absent downloads as e.g. “GID#... is not found”. Keep
	// this deliberately narrow so unrelated RPC errors containing “not found”
	// are not turned into successful cleanup.
	hasObject := strings.Contains(message, "gid") || strings.Contains(message, "download result")
	hasMissing := strings.Contains(message, "not found") || strings.Contains(message, "does not exist")
	return hasObject && hasMissing
}

func isAria2CannotRemove(err error) bool {
	var rpcErr *aria2RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.message)
	return strings.Contains(message, "cannot remove") || strings.Contains(message, "can't remove") || strings.Contains(message, "not removable")
}

func isAria2CannotChangeOption(err error) bool {
	var rpcErr *aria2RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.message)
	return strings.Contains(message, "cannot change option") && strings.Contains(message, "gid")
}

func isAria2CouldNotRemoveResult(err error) bool {
	var rpcErr *aria2RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.message)
	return strings.Contains(message, "could not remove download result") && strings.Contains(message, "gid")
}

const removeWaitTimeout = 5 * time.Second

func (a *Aria2) waitForRemoved(ctx context.Context, id string) error {
	waitCtx, cancel := context.WithTimeout(ctx, removeWaitTimeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status ariaStatus
		err := a.Call(waitCtx, "tellStatus", []any{id}, &status)
		if err != nil {
			if isAria2NotFound(err) {
				return a.Forget(waitCtx, id)
			}
			if waitCtx.Err() != nil {
				return waitCtx.Err()
			}
			return err
		}
		if isTerminalAriaStatus(status.Status) {
			return a.Forget(waitCtx, id)
		}

		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (a *Aria2) removeFollowed(ctx context.Context, parentID string, followed []string) error {
	seen := make(map[string]struct{}, len(followed))
	for _, childID := range followed {
		if childID == "" || childID == parentID {
			continue
		}
		if _, ok := seen[childID]; ok {
			continue
		}
		seen[childID] = struct{}{}
		if err := a.Remove(ctx, childID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Aria2) removeAfterCannotRemove(ctx context.Context, id string, original error) error {
	var status ariaStatus
	if err := a.Call(ctx, "tellStatus", []any{id}, &status); err != nil {
		if isAria2NotFound(err) {
			return a.Forget(ctx, id)
		}
		return original
	}
	if err := a.removeFollowed(ctx, id, status.FollowedBy); err != nil {
		return err
	}
	if isTerminalAriaStatus(status.Status) {
		return a.Forget(ctx, id)
	}
	// The result is still live. Keep the original remove failure instead of
	// forgetting a task that may still be active.
	return original
}

// Forget drops a stopped aria2 result. aria2 returns an error when the GID
// has already disappeared; treating that case as success makes cleanup safe
// to retry and lets Store remove jobs whose engine state is unknown.
func (a *Aria2) Forget(ctx context.Context, id string) error {
	err := a.Call(ctx, "removeDownloadResult", []any{id}, nil)
	if err == nil || isAria2NotFound(err) {
		a.forgetForceSaved(id)
		return nil
	}
	if isAria2CouldNotRemoveResult(err) {
		// aria2 uses the same generic failure for a result that cannot be
		// removed yet and, in some versions, for a GID that has just vanished.
		// Probe the GID before deciding that cleanup is idempotently complete;
		// an active or otherwise visible task must keep the original error.
		var status ariaStatus
		if statusErr := a.Call(ctx, "tellStatus", []any{id}, &status); isAria2NotFound(statusErr) {
			a.forgetForceSaved(id)
			return nil
		}
	}
	return err
}

func (a *Aria2) Remove(ctx context.Context, id string) error {
	// Inspect the result first. remove rejects stopped complete/error/removed
	// entries, while removeDownloadResult is exactly the operation needed for
	// those entries.
	var status ariaStatus
	if err := a.Call(ctx, "tellStatus", []any{id}, &status); err != nil {
		if isAria2NotFound(err) {
			return a.Forget(ctx, id)
		}
		return err
	}
	if err := a.removeFollowed(ctx, id, status.FollowedBy); err != nil {
		return err
	}
	if isTerminalAriaStatus(status.Status) {
		return a.Forget(ctx, id)
	}

	if err := a.Call(ctx, "remove", []any{id}, nil); err != nil {
		if isAria2NotFound(err) {
			return a.Forget(ctx, id)
		}
		if isAria2CannotRemove(err) {
			return a.removeAfterCannotRemove(ctx, id, err)
		}
		return err
	}
	return a.waitForRemoved(ctx, id)
}
func (a *Aria2) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.Call(ctx, "saveSession", nil, nil)
}

func (a *Aria2) SaveSession(ctx context.Context) error { return a.Call(ctx, "saveSession", nil, nil) }

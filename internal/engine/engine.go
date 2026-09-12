package engine

import "context"

type Item struct {
	ID           string   `json:"engine_id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Error        string   `json:"error,omitempty"`
	Total        int64    `json:"total"`
	Completed    int64    `json:"completed"`
	DownloadRate int64    `json:"download_rate"`
	UploadRate   int64    `json:"upload_rate"`
	Progress     float64  `json:"progress"`
	FollowedBy   []string `json:"followed_by,omitempty"`
}

type Backend interface {
	Add(context.Context, string) (string, error)
	List(context.Context) ([]Item, error)
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Remove(context.Context, string) error
	Close() error
}

// Package video adapts website videos to the persistent download queue.
package video

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var tweetPath = regexp.MustCompile(`^/(?:[^/]+/status|i/web/status|statuses)/(\d+)(?:/(?:video|photo)/\d+)?/?$`)
var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var mediaID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Source returns handled=true even for unsupported pages on a supported site:
// those pages must never fall through to an HTML download.
func Source(raw string) (canonical, site, id string, handled bool, err error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "x.com", "www.x.com", "twitter.com", "www.twitter.com", "mobile.twitter.com", "mobile.x.com":
		site = "x"
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "www.youtu.be":
		site = "youtube"
	default:
		return
	}
	handled = true
	if u.User != nil || u.Port() != "" {
		err = errors.New("video links cannot contain credentials or a custom port")
		return
	}
	if site == "x" {
		m := tweetPath.FindStringSubmatch(u.Path)
		if m == nil {
			err = errors.New("provide an X/Twitter post URL containing /status/<id>")
			return
		}
		id = m[1]
		canonical = "https://x.com/i/web/status/" + id
		return
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if host == "youtu.be" || host == "www.youtu.be" {
		if len(parts) == 1 {
			id = parts[0]
		}
	} else if u.Path == "/watch" {
		id = u.Query().Get("v")
	} else if len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "embed" || parts[0] == "live") {
		id = parts[1]
	}
	if !youtubeID.MatchString(id) {
		err = errors.New("provide a YouTube video or Shorts URL; playlists and channels are not supported")
		return
	}
	canonical = "https://www.youtube.com/watch?v=" + id
	return
}

type Spec struct {
	URL     string `json:"url"`
	Site    string `json:"site"`
	PostID  string `json:"post_id"`
	MediaID string `json:"media_id"`
	Index   int    `json:"index"`
	Title   string `json:"title"`
}

func (s Spec) Key() string      { return s.Site + ":" + s.PostID + ":" + s.MediaID }
func (s Spec) Filename() string { return s.Site + "_" + s.PostID + "_" + s.MediaID }
func (s Spec) Validate() error {
	u, site, id, ok, err := Source(s.URL)
	if err != nil || !ok || u != s.URL || site != s.Site || id != s.PostID || !mediaID.MatchString(s.MediaID) || len(s.MediaID) > 128 || s.Index < 1 {
		return errors.New("invalid video task metadata")
	}
	return nil
}

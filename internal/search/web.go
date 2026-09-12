package search

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

const (
	searchHTTPTimeout  = 20 * time.Second
	maxSearchResponse  = 8 << 20
	maxProviderResults = 200
)

var (
	magnetPattern      = regexp.MustCompile(`(?i)magnet:\?[^"'<>[:space:]]+`)
	sizePattern        = regexp.MustCompile(`(?i)^([0-9]+(?:[.,][0-9]+)?)\s*(bytes?|[kmgtpe](?:i?b)?)?$`)
	labeledSizePattern = regexp.MustCompile(`(?i)\b(?:total\s+)?size\s*:\s*([0-9]+(?:[.,][0-9]+)?\s*(?:bytes?|[kmgtpe](?:i?b)?))`)
	seedPattern        = regexp.MustCompile(`(?i)\bseeds?\b\s*[:：]?\s*([0-9][0-9,]*)`)
	peerPattern        = regexp.MustCompile(`(?i)\b(?:leechers?|peers?)\b\s*[:：]?\s*([0-9][0-9,]*)`)
	sourceURLPattern   = regexp.MustCompile(`(?i)https?://[^\s"']+`)
)

// NewHTTPClient creates the bounded client used by web search providers.
// The system trust pool is retained and caFile, when supplied, is appended to
// it.  Redirects are accepted only when the destination remains HTTP(S).
func NewHTTPClient(caFile string) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA bundle contains no certificates")
		}
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
	} else {
		transport = &http.Transport{}
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	if roots != nil {
		transport.TLSClientConfig.RootCAs = roots
	}
	return &http.Client{
		Transport: transport,
		Timeout:   searchHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errors.New("too many redirects")
			}
			if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
				return errors.New("redirect destination must use HTTP(S)")
			}
			return nil
		},
	}, nil
}

// WebProviders turns enabled web source configurations into providers.  The
// native eMule source is deliberately left to the daemon's native provider.
func WebProviders(sources []SourceConfig, client *http.Client) []Provider {
	if client == nil {
		var err error
		client, err = NewHTTPClient("")
		if err != nil {
			return nil
		}
	}
	providers := make([]Provider, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if !source.Enabled || source.Type == "emule" {
			continue
		}
		if _, ok := seen[source.ID]; ok || ValidateSourceConfig(source) != nil {
			continue
		}
		if !webSourceType(source.Type) {
			continue
		}
		seen[source.ID] = struct{}{}
		info := PublicSources([]SourceConfig{source})[0]
		copySource := source
		providers = append(providers, Provider{
			Info: info,
			Search: func(ctx context.Context, req Request, emit Emit) error {
				return searchWebSource(ctx, copySource, client, req, emit)
			},
		})
	}
	return providers
}

func webSourceType(typ string) bool {
	switch typ {
	case "rss", "torznab", "nyaa", "animetosho", "dmhy", "btdig", "linuxtracker", "archive":
		return true
	default:
		return false
	}
}

func expandSourceURL(source SourceConfig, query string) (string, error) {
	if err := ValidateSourceConfig(source); err != nil {
		return "", err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("search query is empty")
	}
	hasQueryPlaceholder := strings.Contains(source.URL, "{query}")
	replacement := url.QueryEscape(query)
	if parsed, parseErr := url.Parse(source.URL); parseErr == nil && strings.Contains(parsed.Path, "{query}") && !strings.Contains(parsed.RawQuery, "{query}") {
		// QueryEscape is correct for a query value, but a path placeholder
		// must escape '/' and spaces using path semantics.
		replacement = url.PathEscape(query)
	}
	raw := strings.Replace(source.URL, "{query}", replacement, 1)
	u, err := url.Parse(raw)
	if err != nil || !validHTTP(raw) {
		return "", errors.New("source URL is invalid after query expansion")
	}
	if source.Type == "torznab" {
		q := u.Query()
		if !hasQueryPlaceholder || q.Get("q") == "" {
			q.Set("q", query)
		}
		if q.Get("t") == "" {
			q.Set("t", "search")
		}
		if source.APIKey != "" && q.Get("apikey") == "" && q.Get("api_key") == "" {
			q.Set("apikey", source.APIKey)
		}
		u.RawQuery = q.Encode()
		raw = u.String()
	}
	return raw, nil
}

func searchWebSource(ctx context.Context, source SourceConfig, client *http.Client, req Request, emit Emit) error {
	target, err := expandSourceURL(source, req.Query)
	if err != nil {
		return err
	}
	body, finalURL, err := fetchSearchResponse(ctx, client, target, source)
	if err != nil {
		return err
	}
	var results []Result
	switch source.Type {
	case "btdig":
		results, err = parseBTDig(body, finalURL)
	case "linuxtracker":
		results, err = parseLinuxTracker(body, finalURL)
	case "archive":
		results, err = parseArchive(body)
	default:
		results, err = parseRSS(body, source.Type, finalURL)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("parse %s response: %s", source.ID, redactSourceString(err.Error(), source))
	}
	if len(results) > maxProviderResults {
		results = results[:maxProviderResults]
	}
	if emit != nil {
		emit(results, 100)
	}
	return nil
}

func fetchSearchResponse(ctx context.Context, client *http.Client, target string, source SourceConfig) ([]byte, *url.URL, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create source request: %w", err)
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, application/json, text/html;q=0.9, */*;q=0.1")
	req.Header.Set("User-Agent", "wirectl-search/1.0")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("source request failed: %s", redactSourceString(err.Error(), source))
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, fmt.Errorf("source returned HTTP status %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSearchResponse {
		return nil, nil, errors.New("source response exceeds 8 MiB")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("read source response: %w", err)
	}
	if len(body) > maxSearchResponse {
		return nil, nil, errors.New("source response exceeds 8 MiB")
	}
	if looksBlockedChallenge(resp.Header.Get("Content-Type"), body) {
		return nil, nil, errors.New("source returned a browser challenge or block page")
	}
	finalURL, _ := url.Parse(target)
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}
	return body, finalURL, nil
}

func looksBlockedChallenge(contentType string, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	if !strings.Contains(strings.ToLower(contentType), "html") && !strings.Contains(lower, "<html") {
		return false
	}
	for _, marker := range []string{
		"checking your browser",
		"checking browser",
		"just a moment...",
		"cf-chl-",
		"verify you are human",
		"enable javascript and cookies",
		"id=\"challenge-form\"",
		"id='challenge-form'",
		"name=\"cf-turnstile-response\"",
		"name='cf-turnstile-response'",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.Contains(lower, "<title>captcha") || strings.Contains(lower, "captcha verification")
}

func redactSourceString(value string, source SourceConfig) string {
	value = sourceURLPattern.ReplaceAllStringFunc(value, func(raw string) string {
		copySource := source
		copySource.URL = raw
		return publicSourceURL(copySource)
	})
	for _, secret := range []string{source.APIKey, url.QueryEscape(source.APIKey)} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	return value
}

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Error *rssError `xml:"error"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssError struct {
	Code        string `xml:"code,attr"`
	Description string `xml:"description,attr"`
}

type rssItem struct {
	Title       string         `xml:"title"`
	Link        string         `xml:"link"`
	Guid        string         `xml:"guid"`
	Description string         `xml:"description"`
	Content     string         `xml:"encoded"`
	Enclosures  []rssEnclosure `xml:"enclosure"`
	InfoHash    string         `xml:"infoHash"`
	Size        string         `xml:"size"`
	Seeders     string         `xml:"seeders"`
	Leechers    string         `xml:"leechers"`
	Attrs       []rssAttr      `xml:"attr"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length string `xml:"length,attr"`
}

type rssAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func parseRSS(body []byte, typ string, base *url.URL) ([]Result, error) {
	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, err
	}
	if feed.XMLName.Local != "rss" || feed.Channel.Error != nil {
		if feed.Channel.Error != nil {
			if code := strings.TrimSpace(feed.Channel.Error.Code); code != "" {
				return nil, fmt.Errorf("indexer returned error code %s", code)
			}
			return nil, errors.New("indexer returned an error")
		}
		return nil, errors.New("response is not an RSS feed")
	}
	results := make([]Result, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		if result, ok := parseRSSItem(item, typ, base); ok {
			results = append(results, result)
		}
	}
	return normalizeProviderResults(results), nil
}

func parseRSSItem(item rssItem, typ string, base *url.URL) (Result, bool) {
	attrs := make(map[string]string, len(item.Attrs))
	for _, attr := range item.Attrs {
		attrs[strings.ToLower(strings.TrimSpace(attr.Name))] = strings.TrimSpace(attr.Value)
	}
	title := cleanText(item.Title)
	description := stdhtml.UnescapeString(item.Description + "\n" + item.Content)
	var magnet string
	var infoHash string
	for _, candidate := range []string{item.Link, item.Guid, item.Description, item.Content} {
		if m := firstMagnet(candidate); m != "" {
			magnet = m
			if h := magnetInfoHash(m); h != "" {
				infoHash = h
			}
			break
		}
	}
	if magnet == "" {
		for _, enclosure := range item.Enclosures {
			if m := firstMagnet(enclosure.URL); m != "" {
				magnet = m
				if h := magnetInfoHash(m); h != "" {
					infoHash = h
				}
				break
			}
		}
	}
	if infoHash == "" {
		for _, candidate := range []string{item.InfoHash, attrs["infohash"], attrs["info_hash"]} {
			if h, err := NormalizeInfoHash(strings.TrimSpace(candidate)); err == nil {
				infoHash = h
				break
			}
		}
	}
	var torrentURL string
	for _, enclosure := range item.Enclosures {
		if u := resolveHTTPLink(enclosure.URL, base); u != "" && (isTorrentURL(u) || strings.Contains(strings.ToLower(enclosure.Type), "bittorrent")) {
			torrentURL = u
			break
		}
	}
	if torrentURL == "" {
		for _, candidate := range []string{item.Link, item.Guid} {
			if u := resolveHTTPLink(candidate, base); u != "" && isTorrentURL(u) {
				torrentURL = u
				break
			}
		}
	}
	if magnet == "" {
		for _, candidate := range extractMagnets(description) {
			if candidate != "" {
				magnet = candidate
				if h := magnetInfoHash(magnet); h != "" {
					infoHash = h
				}
				break
			}
		}
	}
	if magnet == "" && infoHash != "" {
		magnet = magnetFromHash(infoHash, title)
	}
	if magnet == "" && torrentURL == "" {
		return Result{}, false
	}
	if title == "" && magnet != "" {
		if u, err := url.Parse(magnet); err == nil {
			title = cleanText(u.Query().Get("dn"))
		}
	}
	if title == "" && torrentURL != "" {
		if u, err := url.Parse(torrentURL); err == nil {
			title = cleanText(strings.TrimSuffix(pathBase(u.Path), ".torrent"))
		}
	}
	var size int64
	for _, candidate := range []string{attrs["size"], attrs["length"], item.Size} {
		if candidate != "" {
			if n := parseSizeValue(candidate); n > 0 {
				size = n
				break
			}
		}
	}
	if size == 0 {
		for _, enclosure := range item.Enclosures {
			// DMHY publishes a magnet enclosure with length="1".  That is
			// a placeholder and must never become a one-byte torrent.
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(enclosure.URL)), "magnet:") {
				continue
			}
			if n := parseSizeValue(enclosure.Length); n > 1 {
				size = n
				break
			}
		}
	}
	if size == 0 {
		if m := labeledSizePattern.FindStringSubmatch(cleanText(description)); len(m) == 2 {
			size = parseSizeValue(m[1])
		}
	}
	seeds := parseCount(firstNonEmpty(attrs["seeders"], attrs["seeds"], item.Seeders))
	peers := parseCount(firstNonEmpty(attrs["leechers"], attrs["peers"], item.Leechers))
	pageURL := ""
	for _, candidate := range []string{item.Guid, item.Link} {
		if u := resolveHTTPLink(candidate, base); u != "" && !isTorrentURL(u) {
			pageURL = u
			break
		}
	}
	result := Result{Name: title, InfoHash: infoHash, TorrentURL: torrentURL, PageURL: pageURL, Size: size, Seeds: seeds, Peers: peers}
	if magnet != "" {
		result.Link = magnet
	} else {
		result.Link = torrentURL
	}
	return result, true
}

func normalizeProviderResults(results []Result) []Result {
	out := make([]Result, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		normalized, err := NormalizeResult(result)
		if err != nil {
			continue
		}
		if _, ok := seen[normalized.ID]; ok {
			continue
		}
		seen[normalized.ID] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func firstMagnet(value string) string {
	value = stdhtml.UnescapeString(value)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "magnet:") {
		if m := normalizeMagnet(strings.TrimSpace(value)); m != "" {
			return m
		}
	}
	for _, candidate := range magnetPattern.FindAllString(value, -1) {
		candidate = strings.TrimRight(candidate, ",.;)]}")
		if m := normalizeMagnet(candidate); m != "" {
			return m
		}
	}
	return ""
}

func extractMagnets(value string) []string {
	value = stdhtml.UnescapeString(value)
	matches := magnetPattern.FindAllString(value, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		if m := normalizeMagnet(strings.TrimRight(match, ",.;)]}")); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func normalizeMagnet(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") {
		return ""
	}
	if magnetInfoHash(u.String()) == "" {
		return ""
	}
	return u.String()
}

func magnetInfoHash(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	for _, xt := range u.Query()["xt"] {
		if strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
			if h, err := NormalizeInfoHash(strings.TrimPrefix(strings.ToLower(xt), "urn:btih:")); err == nil {
				return h
			}
		}
	}
	return ""
}

func magnetFromHash(hash, title string) string {
	values := url.Values{"xt": {"urn:btih:" + hash}}
	if title != "" {
		values.Set("dn", title)
	}
	return "magnet:?" + values.Encode()
}

func parseArchive(body []byte) ([]Result, error) {
	var response struct {
		Response struct {
			Docs []struct {
				Identifier string `json:"identifier"`
				Title      string `json:"title"`
			} `json:"docs"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(response.Response.Docs))
	for _, doc := range response.Response.Docs {
		identifier := strings.TrimSpace(doc.Identifier)
		if identifier == "" || strings.ContainsAny(identifier, "\r\n") {
			continue
		}
		idPath := url.PathEscape(identifier)
		torrent := "https://archive.org/download/" + idPath + "/" + idPath + "_archive.torrent"
		page := "https://archive.org/details/" + idPath
		results = append(results, Result{Name: cleanText(doc.Title), Link: torrent, TorrentURL: torrent, PageURL: page})
	}
	return normalizeProviderResults(results), nil
}

func parseBTDig(body []byte, base *url.URL) ([]Result, error) {
	doc, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0)
	rows := descendantNodes(doc, func(n *xhtml.Node) bool { return hasClass(n, "one_result") })
	for _, row := range rows {
		titleAnchor := descendantNode(row, func(n *xhtml.Node) bool {
			return n.Type == xhtml.ElementNode && n.Data == "a" && hasClass(n, "torrent_name")
		})
		if titleAnchor == nil {
			nameNode := descendantNode(row, func(n *xhtml.Node) bool { return hasClass(n, "torrent_name") })
			titleAnchor = descendantNode(nameNode, func(n *xhtml.Node) bool { return n.Type == xhtml.ElementNode && n.Data == "a" })
		}
		magnetAnchor := descendantNode(row, func(n *xhtml.Node) bool { return hasClass(n, "torrent_magnet") })
		magnetAnchor = descendantNode(magnetAnchor, func(n *xhtml.Node) bool { return n.Type == xhtml.ElementNode && n.Data == "a" })
		if titleAnchor == nil || magnetAnchor == nil {
			continue
		}
		magnet := firstMagnet(attrValue(magnetAnchor, "href"))
		if magnet == "" {
			continue
		}
		name := cleanText(nodeText(titleAnchor))
		page := resolveHTTPLink(attrValue(titleAnchor, "href"), base)
		sizeNode := descendantNode(row, func(n *xhtml.Node) bool { return hasClass(n, "torrent_size") })
		size := int64(0)
		if sizeNode != nil {
			size = parseSizeValue(nodeText(sizeNode))
		}
		results = append(results, Result{Name: name, Link: magnet, PageURL: page, Size: size, InfoHash: magnetInfoHash(magnet)})
	}
	if len(results) == 0 {
		if len(rows) == 0 && hasInputName(doc, "q") {
			// A structurally valid search page can legitimately contain no
			// torrents.  A generic HTML response without the search form is
			// usually a block page or an upstream template change.
			return nil, nil
		}
		return nil, errors.New("BTDig response did not match the result template")
	}
	return normalizeProviderResults(results), nil
}

func parseLinuxTracker(body []byte, base *url.URL) ([]Result, error) {
	doc, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0)
	seen := make(map[string]struct{})
	sawDetails := false
	for _, row := range descendantNodes(doc, func(n *xhtml.Node) bool { return n.Type == xhtml.ElementNode && n.Data == "tr" }) {
		var titleAnchor, magnetAnchor *xhtml.Node
		for _, anchor := range descendantNodes(row, func(n *xhtml.Node) bool { return n.Type == xhtml.ElementNode && n.Data == "a" }) {
			href := attrValue(anchor, "href")
			title := attrValue(anchor, "title")
			if titleAnchor == nil && strings.HasPrefix(strings.ToLower(title), "view details:") {
				titleAnchor = anchor
			}
			if magnetAnchor == nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(href)), "magnet:") {
				magnetAnchor = anchor
			}
		}
		if titleAnchor == nil || magnetAnchor == nil {
			if titleAnchor != nil {
				sawDetails = true
			}
			continue
		}
		magnet := firstMagnet(attrValue(magnetAnchor, "href"))
		if magnet == "" {
			continue
		}
		key := magnetInfoHash(magnet)
		if key == "" {
			key = magnet
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		name := strings.TrimSpace(strings.TrimPrefix(attrValue(titleAnchor, "title"), "View details:"))
		if name == "" {
			name = cleanText(nodeText(titleAnchor))
		}
		text := nodeText(row)
		result := Result{
			Name:     cleanText(name),
			Link:     magnet,
			InfoHash: magnetInfoHash(magnet),
			PageURL:  resolveHTTPLink(attrValue(titleAnchor, "href"), base),
			Size:     parseLabeledSize(text),
			Seeds:    parseLabeledCount(text, seedPattern),
			Peers:    parseLabeledCount(text, peerPattern),
		}
		results = append(results, result)
	}
	if len(results) == 0 {
		if !sawDetails && hasInputName(doc, "search") {
			return nil, nil
		}
		return nil, errors.New("LinuxTracker response did not match the result template")
	}
	return normalizeProviderResults(results), nil
}

func parseLabeledSize(text string) int64 {
	if m := labeledSizePattern.FindStringSubmatch(text); len(m) == 2 {
		return parseSizeValue(m[1])
	}
	return 0
}

func parseLabeledCount(text string, pattern *regexp.Regexp) int64 {
	if m := pattern.FindStringSubmatch(text); len(m) == 2 {
		return parseCount(m[1])
	}
	return 0
}

func parseSizeValue(raw string) int64 {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\u00a0", " "))
	if raw == "" {
		return 0
	}
	m := sizePattern.FindStringSubmatch(raw)
	if len(m) != 3 {
		return 0
	}
	n, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	if err != nil || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0
	}
	unit := strings.ToLower(m[2])
	multiplier := float64(1)
	switch unit {
	case "", "b", "byte", "bytes":
	case "kb":
		multiplier = 1e3
	case "mb":
		multiplier = 1e6
	case "gb":
		multiplier = 1e9
	case "tb":
		multiplier = 1e12
	case "pb":
		multiplier = 1e15
	case "kib":
		multiplier = 1 << 10
	case "mib":
		multiplier = 1 << 20
	case "gib":
		multiplier = 1 << 30
	case "tib":
		multiplier = 1 << 40
	case "pib":
		multiplier = 1 << 50
	default:
		return 0
	}
	if n > float64(math.MaxInt64)/multiplier {
		return math.MaxInt64
	}
	return int64(math.Round(n * multiplier))
}

func parseCount(raw string) int64 {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), ",", "")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func resolveHTTPLink(raw string, base *url.URL) string {
	raw = stdhtml.UnescapeString(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !u.IsAbs() && base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return u.String()
}

func isTorrentURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	return strings.HasSuffix(path, ".torrent") || strings.Contains(path, ".torrent/")
}

func pathBase(path string) string {
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func cleanText(value string) string {
	value = stdhtml.UnescapeString(value)
	value = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(value, " ")
	return strings.Join(strings.Fields(value), " ")
}

func attrValue(n *xhtml.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func hasClass(n *xhtml.Node, class string) bool {
	for _, value := range strings.Fields(attrValue(n, "class")) {
		if value == class {
			return true
		}
	}
	return false
}

func hasInputName(root *xhtml.Node, name string) bool {
	return descendantNode(root, func(n *xhtml.Node) bool {
		return n.Type == xhtml.ElementNode && n.Data == "input" && strings.EqualFold(attrValue(n, "name"), name)
	}) != nil
}

func nodeText(n *xhtml.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var visit func(*xhtml.Node)
	visit = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode && (node.Data == "script" || node.Data == "style") {
			return
		}
		if node.Type == xhtml.TextNode {
			b.WriteByte(' ')
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func descendantNodes(root *xhtml.Node, predicate func(*xhtml.Node) bool) []*xhtml.Node {
	if root == nil {
		return nil
	}
	out := make([]*xhtml.Node, 0)
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if predicate(n) {
			out = append(out, n)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return out
}

func descendantNode(root *xhtml.Node, predicate func(*xhtml.Node) bool) *xhtml.Node {
	if root == nil {
		return nil
	}
	if predicate(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := descendantNode(child, predicate); found != nil {
			return found
		}
	}
	return nil
}

// Package feeds fetches and parses RSS/Atom feeds and keeps them up to date.
package feeds

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/mmcdole/gofeed"

	"github.com/benaylward/gotimes/internal/store"
)

const (
	maxBody      = 5 << 20
	maxSummary   = 500
	maxContent   = 200 << 10
	fetchTimeout = 15 * time.Second
	maxRedirects = 5
	userAgent    = "gotimes/0.1 (+rss reader)"
)

// Fetcher downloads and parses feeds.
type Fetcher struct {
	client *http.Client
}

// NewFetcher returns a Fetcher. Unless allowPrivate is set, it refuses to
// connect to loopback, private, and link-local addresses so users can't point
// the server at services on its own network. The check runs at dial time, so
// it also covers redirects and DNS rebinding.
func NewFetcher(allowPrivate bool) *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		}
	}
	return &Fetcher{client: &http.Client{
		Timeout:   fetchTimeout,
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil, MaxIdleConnsPerHost: 2},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}
}

// Fetch polls one feed. A conditional GET is used when etag/lastModified are
// known; res.NotModified is set on a 304. Errors are returned in res.Err.
func (f *Fetcher) Fetch(ctx context.Context, st store.FeedState) store.FetchResult {
	res := store.FetchResult{FeedID: st.ID}
	res.Err = f.fetch(ctx, st, &res)
	return res
}

func (f *Fetcher) fetch(ctx context.Context, st store.FeedState, res *store.FetchResult) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.5")
	if st.ETag != "" {
		req.Header.Set("If-None-Match", st.ETag)
	}
	if st.LastModified != "" {
		req.Header.Set("If-Modified-Since", st.LastModified)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	parsed, err := gofeed.NewParser().Parse(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}

	res.Title = strings.TrimSpace(parsed.Title)
	res.SiteURL = parsed.Link
	res.ETag = resp.Header.Get("ETag")
	res.LastModified = resp.Header.Get("Last-Modified")
	res.Items = make([]store.NewItem, 0, len(parsed.Items))
	now := time.Now()
	for _, it := range parsed.Items {
		res.Items = append(res.Items, toItem(it, now))
	}
	return nil
}

func toItem(it *gofeed.Item, now time.Time) store.NewItem {
	pub := now
	switch {
	case it.PublishedParsed != nil:
		pub = *it.PublishedParsed
	case it.UpdatedParsed != nil:
		pub = *it.UpdatedParsed
	}
	if pub.After(now) { // don't let bad dates pin an item to the top forever
		pub = now
	}
	body := it.Content
	if body == "" {
		body = it.Description
	}
	var author string
	if it.Author != nil {
		author = strings.TrimSpace(it.Author.Name)
	}
	return store.NewItem{
		GUID:      guid(it),
		URL:       strings.TrimSpace(it.Link),
		Title:     strings.TrimSpace(html.UnescapeString(it.Title)),
		Summary:   summarize(body),
		Author:    author,
		Content:   sanitize(body),
		Published: pub.UTC(),
	}
}

func guid(it *gofeed.Item) string {
	switch {
	case it.GUID != "":
		return it.GUID
	case it.Link != "":
		return it.Link
	}
	sum := sha1.Sum([]byte(it.Title + it.Published))
	return hex.EncodeToString(sum[:])
}

// sanitizer allows ordinary article markup and strips scripts, event handlers,
// and anything else active. Links open in a new tab without referrer.
var sanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RequireNoReferrerOnLinks(true)
	return p
}()

func sanitize(s string) string {
	if len(s) > maxContent {
		s = s[:maxContent]
	}
	return sanitizer.Sanitize(s)
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)
var spaceRe = regexp.MustCompile(`\s+`)

// summarize strips markup and truncates, so feed HTML is never rendered raw.
func summarize(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > maxSummary {
		s = string(r[:maxSummary]) + "…"
	}
	return s
}

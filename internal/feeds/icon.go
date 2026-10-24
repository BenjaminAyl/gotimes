package feeds

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/benaylward/rss/internal/store"
)

const (
	maxIcon       = 256 << 10
	maxIconPage   = 1 << 20 // the <head> is all we need; the rest of a big page is ignored
	iconRecheck   = 7 * 24 * time.Hour
	iconRetryWait = time.Hour // after a failed check, try again sooner than a week
)

// get downloads a URL through the guarded client and also returns the URL it
// ended up at after redirects. A body longer than limit is cut off if truncate
// is set (enough for a page's head), otherwise it's an error.
func (f *Fetcher) get(ctx context.Context, u string, limit int64, truncate bool) ([]byte, http.Header, *url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, &statusError{resp.Status}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, nil, err
	}
	if int64(len(b)) > limit {
		if !truncate {
			return nil, nil, nil, errTooBig
		}
		b = b[:limit]
	}
	return b, resp.Header, resp.Request.URL, nil
}

type statusError struct{ status string }

func (e *statusError) Error() string { return "unexpected status " + e.status }

var errTooBig = &statusError{"body too large"}

// Icon finds a site's icon: the <link rel="icon"> its page declares, else
// /favicon.ico, else the logo the feed declares (imageURL), else the optional
// IconService. It returns nil if none can be fetched.
func (f *Fetcher) Icon(ctx context.Context, pageURL, imageURL string) ([]byte, string) {
	base, err := url.Parse(pageURL)
	if err != nil || base.Host == "" {
		return nil, ""
	}
	var candidates []string
	// resolve links against where the page ended up, not where we asked: a site may redirect to another host
	if page, _, final, err := f.get(ctx, pageURL, maxIconPage, true); err == nil {
		base = final
		candidates = iconLinks(page, final.String())
	}
	candidates = append(candidates[:min(len(candidates), 3)], base.Scheme+"://"+base.Host+"/favicon.ico")
	if u, ok := resolve(pageURL, imageURL); ok && imageURL != "" {
		candidates = append(candidates, u)
	}
	if f.IconService != "" {
		candidates = append(candidates, fmt.Sprintf(f.IconService, base.Hostname()))
	}
	for _, c := range candidates {
		if b, ct := f.fetchImage(ctx, c); b != nil {
			return b, ct
		}
	}
	return nil, ""
}

// fetchImage downloads one candidate icon, or returns nil if it isn't an image.
func (f *Fetcher) fetchImage(ctx context.Context, u string) ([]byte, string) {
	b, hdr, _, err := f.get(ctx, u, maxIcon, false)
	if err != nil || len(b) == 0 {
		return nil, ""
	}
	if ct := imageType(b, hdr.Get("Content-Type")); ct != "" {
		return b, ct
	}
	return nil, ""
}

// iconLinks orders a page's declared icons: regular icons before touch icons.
func iconLinks(page []byte, pageURL string) []string {
	var regular, touch []string
	for _, l := range linkTags(page) {
		u, ok := resolve(pageURL, l.href)
		if !ok {
			continue
		}
		switch {
		case strings.Contains(l.rel, "mask-icon"):
		case strings.Contains(l.rel, "apple-touch-icon"):
			touch = append(touch, u)
		case strings.Contains(l.rel, "icon"):
			regular = append(regular, u)
		}
	}
	return append(regular, touch...)
}

// imageType returns the image content type of b, or "" if it isn't an image.
func imageType(b []byte, declared string) string {
	if ct, _, _ := strings.Cut(declared, ";"); strings.HasPrefix(ct, "image/") {
		return ct
	}
	if ct := http.DetectContentType(b); strings.HasPrefix(ct, "image/") {
		return ct
	}
	return ""
}

// FillIcon fetches and stores one feed's icon. A failed check is retried sooner
// than a successful one is refreshed.
func FillIcon(ctx context.Context, st *store.Store, f *Fetcher, t store.IconTarget) {
	site := t.SiteURL
	if site == "" {
		site = t.URL
	}
	data, ct := f.Icon(ctx, site, t.ImageURL)
	checked := time.Now()
	if len(data) == 0 {
		checked = checked.Add(-iconRecheck + iconRetryWait)
	}
	if err := st.SaveIcon(ctx, t.ID, data, ct, checked); err != nil && ctx.Err() == nil {
		log.Printf("feed %d: save icon: %v", t.ID, err)
	}
}

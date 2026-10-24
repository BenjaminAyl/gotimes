package feeds

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/benaylward/rss/internal/store"
)

const (
	maxIcon       = 128 << 10
	maxIconPage   = 1 << 20
	iconRecheck   = 7 * 24 * time.Hour
	iconRetryWait = 24 * time.Hour // after a failed check, try again sooner than a week
)

// get downloads up to limit bytes of a URL through the guarded client.
func (f *Fetcher) get(ctx context.Context, u string, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &statusError{resp.Status}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, nil, errTooBig
	}
	return b, resp.Header, nil
}

type statusError struct{ status string }

func (e *statusError) Error() string { return "unexpected status " + e.status }

var errTooBig = &statusError{"body too large"}

// Icon finds a site's icon: the <link rel="icon"> its page declares, else
// /favicon.ico. It returns nil if none can be fetched.
func (f *Fetcher) Icon(ctx context.Context, pageURL string) ([]byte, string) {
	base, err := url.Parse(pageURL)
	if err != nil || base.Host == "" {
		return nil, ""
	}
	var candidates []string
	if page, _, err := f.get(ctx, pageURL, maxIconPage); err == nil {
		candidates = iconLinks(page, pageURL)
	}
	candidates = append(candidates, base.Scheme+"://"+base.Host+"/favicon.ico")
	for _, c := range candidates[:min(len(candidates), 4)] {
		b, hdr, err := f.get(ctx, c, maxIcon)
		if err != nil || len(b) == 0 {
			continue
		}
		if ct := imageType(b, hdr.Get("Content-Type")); ct != "" {
			return b, ct
		}
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
	data, ct := f.Icon(ctx, site)
	checked := time.Now()
	if len(data) == 0 {
		checked = checked.Add(-iconRecheck + iconRetryWait)
	}
	if err := st.SaveIcon(ctx, t.ID, data, ct, checked); err != nil && ctx.Err() == nil {
		log.Printf("feed %d: save icon: %v", t.ID, err)
	}
}

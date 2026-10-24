package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
)

// frontend serves the embedded single-page app. Asset URLs carry a content hash
// (?v=...), so a changed file is a new URL and can't be served stale by a
// browser or by Cloudflare's edge cache, which caches .css/.js by default.
type frontend struct {
	index  []byte
	assets http.Handler
}

func newFrontend(webFS fs.FS) (*frontend, error) {
	v, err := assetVersion(webFS)
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(webFS, "index.html")
	if err != nil {
		return nil, err
	}
	return &frontend{
		index:  bytes.ReplaceAll(index, []byte("__V__"), []byte(v)),
		assets: http.FileServerFS(webFS),
	}, nil
}

func assetVersion(webFS fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(webFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10], err
}

func (f *frontend) serveIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(f.index)
}

// serveAssets caches versioned URLs forever; anything else must revalidate.
func (f *frontend) serveAssets(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	f.assets.ServeHTTP(w, r)
}

package api

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
)

const placeholderPage = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>TitoMail</title></head>
<body style="font-family:sans-serif;max-width:40em;margin:4em auto">
<h1>TitoMail</h1><p>The server is running, but the web interface was not included in this build.
Run <code>make web</code> (or <code>npm run build</code> in <code>web/</code>) and rebuild the server.</p></body></html>`

// staticFile is a cached, optionally precompressed, file of the web UI.
type staticFile struct {
	data, gz    []byte
	contentType string
}

// static serves the single-page web application. Unknown paths without a
// file extension return index.html so client-side routes can be reloaded.
func (a *API) static() http.Handler {
	if a.opts.Static == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(placeholderPage))
		})
	}
	var cache sync.Map
	load := func(name string) (*staticFile, bool) {
		if v, ok := cache.Load(name); ok {
			return v.(*staticFile), true
		}
		data, err := fs.ReadFile(a.opts.Static, name)
		if err != nil {
			return nil, false
		}
		f := &staticFile{data: data, contentType: mime.TypeByExtension(path.Ext(name))}
		if f.contentType == "" {
			f.contentType = http.DetectContentType(data)
		}
		if compressible(f.contentType) && len(data) > 1024 {
			var b bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
			_, _ = zw.Write(data)
			_ = zw.Close()
			if b.Len() < len(data) {
				f.gz = b.Bytes()
			}
		}
		cache.Store(name, f)
		return f, true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		cacheControl := "no-cache"
		f, found := (*staticFile)(nil), false
		if name != "" && name != "index.html" {
			if f, found = load(name); found && strings.HasPrefix(name, "assets/") {
				cacheControl = "public, max-age=31536000, immutable"
			}
			if !found && path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		if !found {
			if f, found = load("index.html"); !found {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(placeholderPage))
				return
			}
		}
		h := w.Header()
		h.Set("Content-Type", f.contentType)
		h.Set("Cache-Control", cacheControl)
		h.Add("Vary", "Accept-Encoding")
		body := f.data
		if f.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			body = f.gz
		}
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}

func compressible(ct string) bool {
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "javascript") || strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg") || strings.Contains(ct, "xml")
}

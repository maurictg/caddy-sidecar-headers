// Package sidecarheaders adds sidecar metadata to Caddy static file responses.
package sidecarheaders

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

const (
	defaultHints   = ".hints"
	defaultHeaders = ".headers"
	maxFileSize    = 64 << 10
	maxCacheSize   = 1024
)

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("sidecar_headers", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("sidecar_headers", httpcaddyfile.Before, "file_server")
}

// Handler reads optional header files next to static files. It must run before
// file_server. Caddy's root and fs request variables determine the filesystem.
type Handler struct {
	HintsExtension   string `json:"hints_extension,omitempty"`
	HeadersExtension string `json:"headers_extension,omitempty"`

	fsmap caddy.FileSystems
	cache *metadataCache
}

type metadataCache struct {
	sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	size    int64
	modTime time.Time
	header  http.Header
}

func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sidecar_headers",
		New: func() caddy.Module { return new(Handler) },
	}
}

func (h *Handler) Provision(ctx caddy.Context) error {
	h.defaults()
	h.fsmap = ctx.FileSystems()
	h.cache = &metadataCache{entries: make(map[string]cacheEntry)}
	return nil
}

func (h *Handler) defaults() {
	if h.HintsExtension == "" {
		h.HintsExtension = defaultHints
	}
	if h.HeadersExtension == "" {
		h.HeadersExtension = defaultHeaders
	}
}

func (h *Handler) Validate() error {
	if !validExtension(h.HintsExtension) {
		return errors.New("hints_extension must start with a dot and contain no path separator")
	}
	if !validExtension(h.HeadersExtension) {
		return errors.New("headers_extension must start with a dot and contain no path separator")
	}
	if h.HintsExtension == h.HeadersExtension {
		return errors.New("sidecar extensions must differ")
	}
	return nil
}

func validExtension(extension string) bool {
	return strings.HasPrefix(extension, ".") && !strings.ContainsAny(extension, `/\\`)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	requestPath := strings.ToLower(path.Clean(strings.ReplaceAll(r.URL.Path, `\`, "/")))
	if strings.HasSuffix(requestPath, strings.ToLower(h.HintsExtension)) ||
		strings.HasSuffix(requestPath, strings.ToLower(h.HeadersExtension)) {
		return caddyhttp.Error(http.StatusNotFound, nil)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return next.ServeHTTP(w, r)
	}

	fileSystem, key, filename := h.resolve(r)
	if filename == "" {
		return next.ServeHTTP(w, r)
	}
	rw := &responseWriter{
		ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w},
		handler:               h,
		fileSystem:            fileSystem,
		cacheKey:              key,
		filename:              filename,
	}
	return next.ServeHTTP(rw, r)
}

// resolve uses the same root, filesystem and safe path join as file_server.
// The request URI must name a file; implicit directory indexes are private to
// file_server and deliberately not guessed here.
func (h *Handler) resolve(r *http.Request) (fs.FS, string, string) {
	repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if !ok {
		return nil, "", ""
	}
	root := repl.ReplaceAll("{http.vars.root}", ".")
	fsName := repl.ReplaceAll("{http.vars.fs}", "")
	fileSystem, ok := h.fsmap.Get(fsName)
	if !ok {
		return nil, "", ""
	}

	filename := strings.TrimSuffix(caddyhttp.SanitizedPathJoin(root, r.URL.Path), "/")
	info, err := fs.Stat(fileSystem, filename)
	if err != nil {
		return nil, "", ""
	}
	if info.IsDir() {
		return nil, "", ""
	}
	return fileSystem, fsName, filename
}

type responseWriter struct {
	*caddyhttp.ResponseWriterWrapper
	handler    *Handler
	fileSystem fs.FS
	cacheKey   string
	filename   string
	wroteFinal bool
}

func (w *responseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriterWrapper.WriteHeader(status)
		return
	}
	if w.wroteFinal {
		return
	}
	w.wroteFinal = true
	if status == http.StatusNotModified || (status >= 200 && status < 300) {
		w.applySidecars()
	}
	w.ResponseWriterWrapper.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteFinal {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriterWrapper.Write(p)
}

func (w *responseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteFinal {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriterWrapper.ReadFrom(r)
}

func (w *responseWriter) applySidecars() {
	if hints := w.handler.load(w.fileSystem, w.cacheKey, sidecarName(w.filename, w.handler.HintsExtension), true); len(hints) > 0 {
		writeEarlyHints(w.ResponseWriterWrapper, hints)
	}
	if headers := w.handler.load(w.fileSystem, w.cacheKey, sidecarName(w.filename, w.handler.HeadersExtension), false); len(headers) > 0 {
		for name, values := range headers {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
	}
}

func sidecarName(filename, extension string) string {
	return filename + extension
}

// A 1xx write does not clear Go's header map. Temporarily expose only Link so
// final headers do not leak into the informational response.
func writeEarlyHints(w http.ResponseWriter, hints http.Header) {
	final := w.Header().Clone()
	clear(w.Header())
	for _, value := range hints.Values("Link") {
		w.Header().Add("Link", value)
	}
	w.WriteHeader(http.StatusEarlyHints)
	clear(w.Header())
	for name, values := range final {
		w.Header()[name] = values
	}
}

// load stats on every request for immediate development invalidation, but only
// rereads a sidecar when its size or modification time changed.
func (h *Handler) load(fileSystem fs.FS, fsName, filename string, hintsOnly bool) http.Header {
	info, err := fs.Stat(fileSystem, filename)
	if err != nil {
		return nil
	}
	key := fsName + "\x00" + filename

	h.cache.Lock()
	entry, found := h.cache.entries[key]
	h.cache.Unlock()
	if found && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return entry.header.Clone()
	}

	if info.IsDir() || info.Size() > maxFileSize {
		h.cache.store(key, cacheEntry{size: info.Size(), modTime: info.ModTime()})
		return nil
	}
	file, err := fileSystem.Open(filename)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil || len(data) > maxFileSize {
		return nil
	}
	entry.header, _ = parseHeaders(data, hintsOnly)
	entry.size, entry.modTime = info.Size(), info.ModTime()
	h.cache.store(key, entry)
	return entry.header.Clone()
}

func (c *metadataCache) store(key string, entry cacheEntry) {
	c.Lock()
	defer c.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxCacheSize {
		clear(c.entries)
	}
	c.entries[key] = entry
}

func parseHeaders(data []byte, hintsOnly bool) (http.Header, error) {
	headers := make(http.Header)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), maxFileSize+1)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = http.CanonicalHeaderKey(name), strings.TrimSpace(value)
		if !ok || name == "" || !validHeaderValue(value) ||
			(hintsOnly && name != "Link") || (!hintsOnly && !headersAllowed[name]) {
			return nil, fmt.Errorf("invalid or disallowed header on line %d", lineNumber)
		}
		headers.Add(name, value)
	}
	return headers, scanner.Err()
}

func validHeaderValue(value string) bool {
	for i := range len(value) {
		if value[i] == 0x7f || value[i] < 0x20 && value[i] != '\t' {
			return false
		}
	}
	return true
}

// Framing, routing, cookies, validators and representation headers are omitted
// because they could conflict with file_server.
var headersAllowed = map[string]bool{
	"Cache-Control": true, "Content-Disposition": true, "Content-Language": true,
	"Content-Security-Policy": true, "Content-Security-Policy-Report-Only": true,
	"Cross-Origin-Embedder-Policy": true, "Cross-Origin-Opener-Policy": true,
	"Cross-Origin-Resource-Policy": true, "Expires": true, "Link": true,
	"Permissions-Policy": true, "Referrer-Policy": true, "Reporting-Endpoints": true,
	"Strict-Transport-Security": true, "Vary": true, "X-Content-Type-Options": true,
	"X-Frame-Options": true, "X-Xss-Protection": true,
}

func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "hints_extension":
			if !d.Args(&h.HintsExtension) {
				return d.ArgErr()
			}
		case "headers_extension":
			if !d.Args(&h.HeadersExtension) {
				return d.ArgErr()
			}
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ io.ReaderFrom               = (*responseWriter)(nil)
)

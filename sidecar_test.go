package sidecarheaders

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

type osFileSystem struct{}

func (osFileSystem) Open(name string) (fs.File, error) { return os.Open(name) }
func (osFileSystem) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

type countingFileSystem struct {
	osFileSystem
	opens map[string]int
}

func (f *countingFileSystem) Open(name string) (fs.File, error) {
	f.opens[name]++
	return f.osFileSystem.Open(name)
}

type fileSystems struct{ fileSystem fs.FS }

func (f *fileSystems) Register(string, fs.FS) {}
func (f *fileSystems) Unregister(string)      {}
func (f *fileSystems) Get(name string) (fs.FS, bool) {
	return f.fileSystem, name == ""
}
func (f *fileSystems) Default() fs.FS { return f.fileSystem }

type recordedHeader struct {
	status int
	header http.Header
}

type recordingWriter struct {
	header  http.Header
	records []recordedHeader
	body    strings.Builder
	final   bool
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: make(http.Header)}
}

func (w *recordingWriter) Header() http.Header { return w.header }
func (w *recordingWriter) WriteHeader(status int) {
	if w.final {
		return
	}
	w.records = append(w.records, recordedHeader{status: status, header: w.header.Clone()})
	if status < 100 || status > 199 {
		w.final = true
	}
}
func (w *recordingWriter) Write(data []byte) (int, error) {
	if !w.final {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(data)
}

func TestNoSidecars(t *testing.T) {
	result := serve(t, http.MethodGet, nil, nil)
	assertStatuses(t, result, http.StatusOK)
	if got := result.header.Values("Link"); len(got) != 0 {
		t.Fatalf("unexpected Link headers: %v", got)
	}
	if result.body.String() != "hello" {
		t.Fatalf("body = %q", result.body.String())
	}
}

func TestHints(t *testing.T) {
	result := serve(t, http.MethodGet, []byte("Link: </app.css>; rel=preload; as=style\n"), nil)
	assertStatuses(t, result, http.StatusEarlyHints, http.StatusOK)
	if got := result.records[0].header.Values("Link"); !reflect.DeepEqual(got, []string{"</app.css>; rel=preload; as=style"}) {
		t.Fatalf("early Link headers = %v", got)
	}
	if got := result.records[1].header.Values("Link"); len(got) != 0 {
		t.Fatalf("hints leaked into final response: %v", got)
	}
}

func TestRealHTTPServerSends103Then200(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "page.html")
	writeFile(t, main, []byte("hello"))
	writeFile(t, main+".hints", []byte("Link: </app.css>; rel=preload; as=style\n"))
	handler := testHandler()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = caddyRequest(r, root)
		err := handler.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			http.ServeFile(w, r, main)
			return nil
		}))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	var informational []int
	req, err := http.NewRequest(http.MethodGet, server.URL+"/page.html", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			informational = append(informational, code)
			if got := header.Get("Link"); got != "</app.css>; rel=preload; as=style" {
				t.Errorf("early Link header = %q", got)
			}
			return nil
		},
	}))
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !reflect.DeepEqual(informational, []int{http.StatusEarlyHints}) {
		t.Fatalf("informational statuses = %v", informational)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d", response.StatusCode)
	}
}

func TestResponseHeaders(t *testing.T) {
	result := serve(t, http.MethodGet, nil, []byte("Cache-Control: public, max-age=60\nX-Content-Type-Options: nosniff\n"))
	assertStatuses(t, result, http.StatusOK)
	if got := result.records[0].header.Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := result.records[0].header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
}

func TestMultipleLinkHeaders(t *testing.T) {
	hints := []byte("Link: </a.css>; rel=preload; as=style\nLink: </a.js>; rel=preload; as=script\n")
	headers := []byte("Link: </one>; rel=preload\nLink: </two>; rel=preload\n")
	result := serve(t, http.MethodGet, hints, headers)
	assertStatuses(t, result, http.StatusEarlyHints, http.StatusOK)
	if got := result.records[0].header.Values("Link"); !reflect.DeepEqual(got, []string{"</a.css>; rel=preload; as=style", "</a.js>; rel=preload; as=script"}) {
		t.Fatalf("early Link headers = %v", got)
	}
	if got := result.records[1].header.Values("Link"); !reflect.DeepEqual(got, []string{"</one>; rel=preload", "</two>; rel=preload"}) {
		t.Fatalf("final Link headers = %v", got)
	}
}

func TestHEAD(t *testing.T) {
	result := serve(t, http.MethodHead, []byte("Link: </app.css>; rel=preload\n"), []byte("Cache-Control: no-cache\n"))
	assertStatuses(t, result, http.StatusEarlyHints, http.StatusOK)
	if result.body.Len() != 0 {
		t.Fatalf("HEAD response has body %q", result.body.String())
	}
	if got := result.records[1].header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestMalformedAndInjectionAreRejectedAtomically(t *testing.T) {
	headers := []byte("Cache-Control: public\nX-Content-Type-Options: nosniff\rInjected: yes\n")
	result := serve(t, http.MethodGet, nil, headers)
	assertStatuses(t, result, http.StatusOK)
	if got := result.records[0].header.Get("Cache-Control"); got != "" {
		t.Fatalf("valid header from malformed file was partially applied: %q", got)
	}
	if got := result.records[0].header.Get("Injected"); got != "" {
		t.Fatalf("injected header was applied: %q", got)
	}
}

func TestCustomHeadersPassAndDeniedHeadersAreSkipped(t *testing.T) {
	result := serve(t, http.MethodGet, nil, []byte("X-Custom-Header: works\nContent-Length: 999\nSet-Cookie: secret=yes\n"))
	assertStatuses(t, result, http.StatusOK)
	if got := result.records[0].header.Get("X-Custom-Header"); got != "works" {
		t.Fatalf("custom header = %q", got)
	}
	if got := result.records[0].header.Get("Content-Length"); got == "999" {
		t.Fatal("denied Content-Length was applied")
	}
	if got := result.records[0].header.Get("Set-Cookie"); got != "" {
		t.Fatalf("denied Set-Cookie was applied: %q", got)
	}
}

func TestMissingFile(t *testing.T) {
	root := t.TempDir()
	handler := testHandler()
	req := caddyRequest(httptest.NewRequest(http.MethodGet, "http://example.test/missing.html", nil), root)
	w := newRecordingWriter()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		http.NotFound(w, req)
		return nil
	})
	if err := handler.ServeHTTP(w, req, next); err != nil {
		t.Fatal(err)
	}
	assertStatuses(t, w, http.StatusNotFound)
}

func TestSidecarsCannotBeServedDirectly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "page.html.hints"), []byte("secret"))
	handler := testHandler()
	for _, requestPath := range []string{"/page.html.hints", "/page.html.hints/.", "/PAGE.HTML.HINTS"} {
		req := caddyRequest(httptest.NewRequest(http.MethodGet, "http://example.test"+requestPath, nil), root)
		err := handler.ServeHTTP(newRecordingWriter(), req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
			t.Fatal("next handler must not be called")
			return nil
		}))
		handlerErr, ok := err.(caddyhttp.HandlerError)
		if !ok || handlerErr.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: error = %#v, want Caddy 404", requestPath, err)
		}
	}
}

func TestRangeKeepsFileServerSemantics(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "page.html")
	writeFile(t, main, []byte("hello"))
	writeFile(t, main+".headers", []byte("Cache-Control: public\n"))
	handler := testHandler()
	req := caddyRequest(httptest.NewRequest(http.MethodGet, "http://example.test/page.html", nil), root)
	req.Header.Set("Range", "bytes=1-3")
	w := newRecordingWriter()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		http.ServeFile(w, r, main)
		return nil
	})
	if err := handler.ServeHTTP(w, req, next); err != nil {
		t.Fatal(err)
	}
	assertStatuses(t, w, http.StatusPartialContent)
	if w.body.String() != "ell" || w.records[0].header.Get("Cache-Control") != "public" {
		t.Fatalf("body=%q headers=%v", w.body.String(), w.records[0].header)
	}
}

func TestRedirectAndErrorResponsesDoNotGetSidecars(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "redirect", status: http.StatusPermanentRedirect},
		{name: "error", status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			main := filepath.Join(root, "page.html")
			writeFile(t, main, []byte("hello"))
			writeFile(t, main+".hints", []byte("Link: </app.css>; rel=preload\n"))
			writeFile(t, main+".headers", []byte("Cache-Control: public\n"))
			handler := testHandler()
			req := caddyRequest(httptest.NewRequest(http.MethodGet, "http://example.test/page.html", nil), root)
			w := newRecordingWriter()
			next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
				w.WriteHeader(test.status)
				return nil
			})
			if err := handler.ServeHTTP(w, req, next); err != nil {
				t.Fatal(err)
			}
			assertStatuses(t, w, test.status)
			if got := w.records[0].header.Get("Cache-Control"); got != "" {
				t.Fatalf("sidecar header on status %d: %q", test.status, got)
			}
		})
	}
}

func TestNonHTMLFileSidecars(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "feed.xml")
	writeFile(t, main, []byte("hello"))
	writeFile(t, main+".headers", []byte("Cache-Control: public\n"))
	handler := testHandler()
	req := caddyRequest(httptest.NewRequest(http.MethodGet, "http://example.test/feed.xml", nil), root)
	w := newRecordingWriter()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		http.ServeFile(w, r, main)
		return nil
	})
	if err := handler.ServeHTTP(w, req, next); err != nil {
		t.Fatal(err)
	}
	assertStatuses(t, w, http.StatusOK)
	if got := w.records[0].header.Get("Cache-Control"); got != "public" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestCacheRevalidatesWithoutRereadingUnchangedFile(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "page.html")
	writeFile(t, main, []byte("hello"))
	writeFile(t, main+".headers", []byte("Cache-Control: public\n"))
	handler := testHandler()
	countingFS := &countingFileSystem{opens: make(map[string]int)}
	handler.fsmap = &fileSystems{fileSystem: countingFS}

	for range 2 {
		result := serveWith(t, handler, http.MethodGet, main)
		assertStatuses(t, result, http.StatusOK)
		if result.records[0].header.Get("Cache-Control") != "public" {
			t.Fatal("cached header missing")
		}
	}
	if got := countingFS.opens[main+".headers"]; got != 1 {
		t.Fatalf("sidecar open count = %d, want 1", got)
	}
}

func serve(t *testing.T, method string, hints, headers []byte) *recordingWriter {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "page.html")
	writeFile(t, main, []byte("hello"))
	if hints != nil {
		writeFile(t, main+".hints", hints)
	}
	if headers != nil {
		writeFile(t, main+".headers", headers)
	}
	return serveWith(t, testHandler(), method, main)
}

func serveWith(t *testing.T, handler *Handler, method, main string) *recordingWriter {
	t.Helper()
	req := caddyRequest(httptest.NewRequest(method, "http://example.test/page.html", nil), filepath.Dir(main))
	w := newRecordingWriter()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		http.ServeFile(w, r, main)
		return nil
	})
	if err := handler.ServeHTTP(w, req, next); err != nil {
		t.Fatal(err)
	}
	return w
}

func testHandler() *Handler {
	h := &Handler{
		HintsExtension:   defaultHints,
		HeadersExtension: defaultHeaders,
		fsmap:            &fileSystems{fileSystem: osFileSystem{}},
		cache:            &metadataCache{entries: make(map[string]cacheEntry)},
	}
	return h
}

func caddyRequest(req *http.Request, root string) *http.Request {
	repl := caddy.NewReplacer()
	repl.Set("http.vars.root", root)
	repl.Set("http.vars.fs", "")
	ctx := context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl)
	return req.WithContext(ctx)
}

func writeFile(t *testing.T, name string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(name, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertStatuses(t *testing.T, w *recordingWriter, want ...int) {
	t.Helper()
	got := make([]int, len(w.records))
	for i, record := range w.records {
		got[i] = record.status
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
}

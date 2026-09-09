# Caddy sidecar headers

`sidecar_headers` is a small Caddy 2 HTTP middleware for static-file metadata.
It leaves file serving to Caddy's standard `file_server`, while reading two
optional files next to the selected file:

- `page.html.hints` is sent once as `103 Early Hints` (only `Link` is allowed).
- `page.html.headers` is appended to the final response headers.

Missing sidecars are normal. Sidecars are blocked from direct HTTP access.
The module is a regular Caddy module and has no FrankenPHP dependency, so the
same package can be compiled into a FrankenPHP custom build.

The plugin code imports only the Go standard library and Caddy. Entries such as
`go.uber.org/zap` shown as `// indirect` in `go.mod` belong to Caddy's own module
graph; this plugin neither imports nor adds them.

## Build and install

Install current Go and xcaddy, then build Caddy with the module:

```sh
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy build v2.11.4 \
  --with github.com/maurictg/caddy-sidecar-headers@latest
sudo install -m 0755 ./caddy /usr/local/bin/caddy
caddy list-modules | grep sidecar_headers
```

During local development, point xcaddy at the checkout:

```sh
xcaddy build v2.11.4 \
  --with github.com/maurictg/caddy-sidecar-headers=.
```

For FrankenPHP, use its builder image and add one `--with` line to the standard
custom-module recipe:

```dockerfile
FROM dunglas/frankenphp:builder AS builder
COPY --from=caddy:builder /usr/bin/xcaddy /usr/bin/xcaddy
RUN CGO_ENABLED=1 \
    XCADDY_SETCAP=1 \
    XCADDY_GO_BUILD_FLAGS="-ldflags='-w -s' -tags=nobadger,nomysql,nopgx" \
    CGO_CFLAGS=$(php-config --includes) \
    CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" \
    xcaddy build --output /usr/local/bin/frankenphp \
      --with github.com/dunglas/frankenphp=./ \
      --with github.com/dunglas/frankenphp/caddy=./caddy/ \
      --with github.com/maurictg/caddy-sidecar-headers@latest

FROM dunglas/frankenphp AS runner
COPY --from=builder /usr/local/bin/frankenphp /usr/local/bin/frankenphp
```

No FrankenPHP-specific configuration or API is required.

## Caddyfile

```caddyfile
example.com {
    root * /public

    sidecar_headers {
        hints_extension .hints
        headers_extension .headers
    }

    file_server
}
```

The directive registers itself immediately before `file_server` in Caddy's
directive order. Keep it directly before the file server when using a `route`
block.

The defaults are the values shown above, so `sidecar_headers` without a block is
also valid. The configured strings are appended as suffixes: `page.html` maps
to `page.html.hints`, while `feed.xml` maps to `feed.xml.hints`. Every file type
works the same way.

Use Caddy's standard `root` and `fs` directives; their request variables are
shared with this middleware. Rewrites performed before it are respected.
Implicit directory indexes are intentionally not guessed: Caddy does not expose
the selected `file_server { index ... }` file to preceding middleware. Rewrite
to the concrete index file before `sidecar_headers` if its sidecars are needed.

The cache is intentionally not configurable: every request stats a sidecar for
immediate development invalidation, while unchanged files are not reopened or
reread. It holds at most 1024 entries and resets on configuration reload.

## Sidecar format and security

Each non-empty line is `Name: value`; repeated names remain separate values.
Lines beginning with `#` are comments. A malformed or disallowed line rejects
the whole sidecar without failing the static response. Files larger than 64 KiB
are rejected.

Hints files accept only `Link`. Response-header files use a conservative
allowlist:

```text
Cache-Control
Content-Disposition
Content-Language
Content-Security-Policy
Content-Security-Policy-Report-Only
Cross-Origin-Embedder-Policy
Cross-Origin-Opener-Policy
Cross-Origin-Resource-Policy
Expires
Link
Permissions-Policy
Referrer-Policy
Reporting-Endpoints
Strict-Transport-Security
Vary
X-Content-Type-Options
X-Frame-Options
X-XSS-Protection
```

Framing, routing, cookies, authentication, validators, and representation
headers are intentionally refused so sidecars cannot interfere with Caddy's
range handling, redirects, or file validation.

## Verify

```sh
gofmt -w sidecar.go sidecar_test.go
go test ./...
go vet ./...
```

With an HTTP/1.1 or HTTP/2 server running, inspect both responses with:

```sh
curl --http2 -i https://example.com/foo.html
```

Some clients and intermediaries do not display informational responses even
though the final response is received normally.

## Example

The [`example`](example/) directory contains a runnable Caddyfile, HTML, CSS,
Early Hints sidecar and response-header sidecar. From the repository root:

```sh
xcaddy build v2.11.4 \
  --with github.com/maurictg/caddy-sidecar-headers=.
./caddy run --config example/Caddyfile
curl --http1.1 -i http://localhost:8080/index.html
```

The output contains a `103 Early Hints` block followed by the final `200 OK`
response. Request `/index.html` explicitly because implicit directory-index
selection belongs to `file_server` and is not exposed to this middleware.

## Behavior notes

- `GET` and `HEAD` are supported; HEAD never gains a response body.
- Metadata is attached to successful static responses, including `206` ranges
  and `304 Not Modified`, but never to redirects or errors.
- Any regular static file type is supported. The current URI must resolve to a
  file; implicit directory-index selection remains entirely with `file_server`.
- Early Hints are emitted through Go/Caddy's standard `WriteHeader(103)` path.
  Final response headers are kept out of the informational response.

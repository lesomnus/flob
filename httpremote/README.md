# Ordered HTTP read origins

This module selects an origin before calling `http.Client.Do`. The returned
`*httpremote.Client` implements `Do`, `Get` and `Head`; it is not an
`*http.Client`. Flob's `HttpStores.Client` accepts its `Do` interface.

`NewClient(client, Config{Targets: targets})` copies an HTTP client and retries GET/HEAD on the
next configured base URL only when the transport fails. HTTP responses,
including 401, 403, 404 and 5xx, stop selection. Cancellation and writes never
retry. The default transport bounds dialing, TLS and response-header waits.

Responses stream normally. A request with If-Match stays on its pinned origin;
an interrupted response body is returned to the caller rather than spliced with
bytes from another origin. A caller may restart its content-addressed download
and verify the final digest. Relative redirects retain the actual origin and
credentials are stripped when switching origins. Redirect failures do not restart
the original request at another base. A supplied `http.Client.Timeout` covers
all attempts and reading the response body. Requests passed to `Do` are cloned.
`Config.WrapTransport` can add tracing without replacing the default timeout
configuration. A supplied custom transport keeps its own timeout policy.

## Authentication

`Config.Auth` maps origins to `Auth{Kind, Value}` providers. An origin is the
scheme, hostname and effective port, without a path or query. Hostnames are case
insensitive; omitted ports are 443 for HTTPS and 80 for HTTP. Supported kinds are
`bearer` (a token) and `basic` (an unencoded `username:password` pair).

`Value` is a function called for every actual request, including redirects. It
must be safe for concurrent use. This allows a configuration layer to supply a
literal, environment reference or rotating secret file. A provider error or an
invalid credential is terminal and never triggers failover. Configured auth
overrides the request's Authorization header for that origin; an unconfigured
origin receives no configured credential.

```go
client, err := httpremote.NewClient(baseClient, httpremote.Config{
    Targets: []string{"https://primary.example/blobs", "https://backup.example/blobs"},
    Auth: map[string]httpremote.Auth{
        "https://primary.example": {
            Kind: "bearer",
            Value: func() (string, error) { return os.Getenv("PRIMARY_BLOB_TOKEN"), nil },
        },
    },
})
```

On failover or a cross-origin redirect, explicit Authorization, Proxy-Authorization,
Cookie and custom Host values are removed. Returning to an earlier origin does
not restore those explicit headers. An explicitly configured credential for the new
origin may then be applied. CookieJar cookies continue to follow normal cookie
domain, path and security rules; the jar is consulted and updated for the actual
request URL. Cookies are not isolated by port. The original CheckRedirect hook
is preserved and runs after header stripping; it may deliberately set new headers.

The CLI uses `kind` and `value`, with `value` decoded by `xli/cfg.Secret`. It
accepts literals, `${env:NAME}` and `${file:/path}`. A reference fills the whole
value. Literal credentials are redacted in `flob conf`; references are preserved.
Secret files are rechecked per request so rotated files take effect without
rebuilding the client. Environment references resolve when configuration is read.

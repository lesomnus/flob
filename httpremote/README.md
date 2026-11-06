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

`Config.BearerTokens` maps origins to tokens. An origin is the scheme, hostname
and effective port, without a path or query. Hostnames are case insensitive;
omitted ports are 443 for HTTPS and 80 for HTTP. Tokens are copied at construction
and applied separately to each actual request, including redirects. A configured
token overrides a request's Authorization header for that origin. An unconfigured
origin receives no configured token.

```go
client, err := httpremote.NewClient(baseClient, httpremote.Config{
    Targets: []string{"https://primary.example/blobs", "https://backup.example/blobs"},
    BearerTokens: map[string]string{
        "https://primary.example": os.Getenv("PRIMARY_BLOB_TOKEN"),
        "https://backup.example": os.Getenv("BACKUP_BLOB_TOKEN"),
    },
})
```

On failover or a cross-origin redirect, explicit Authorization, Proxy-Authorization,
Cookie and custom Host values are removed. Returning to an earlier origin does
not restore those explicit headers. An explicitly configured token for the new
origin may then be applied. CookieJar cookies continue to follow normal cookie
domain, path and security rules; the jar is consulted and updated for the actual
request URL. Cookies are not isolated by port. The original CheckRedirect hook
is preserved and runs after header stripping; it may deliberately set new headers.

The CLI reads origin tokens through `bearer_token_env` references. Missing or
empty referenced variables fail configuration instead of silently omitting auth.

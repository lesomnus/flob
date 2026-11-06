# Ordered HTTP read origins

This small module is independent of flob's storage interfaces so readers using
different flob versions can share the same origin selection policy.

`NewClient(client, targets)` copies an HTTP client and retries GET/HEAD on the
next configured base URL only when the transport fails. HTTP responses,
including 401, 403, 404 and 5xx, stop selection. Cancellation and writes never
retry. The default transport bounds dialing, TLS and response-header waits.

Responses stream normally. A request with If-Match stays on its pinned origin;
an interrupted response body is returned to the caller rather than spliced with
bytes from another origin. A caller may restart its content-addressed download
and verify the final digest. Relative redirects retain the actual origin and
credentials are stripped when switching origins.

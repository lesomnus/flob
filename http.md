# HTTP API

`HttpHandler` exposes [Stores]-backed [Store] operations as a RESTful interface.
The store is selected by the first path segment.

URL structure:

```
/{store-id}[/{digest}]
```

A digest is a 64-character lowercase hex-encoded SHA-256 string.

## Metadata Headers

Blob metadata is conveyed through standard HTTP headers:

```
ETag: "<digest>"    SHA-256 digest of the blob (quoted per RFC 7232)
Content-Length: N   Size of the blob in bytes
```

Labels in requests (POST, PATCH) use a `Flob-` prefix to avoid collision with standard HTTP headers.
In responses, labels are returned without the prefix as plain headers.

For example, a blob labeled with `Foo: bar` is sent in a request as:

```
Flob-Foo: bar
```

And returned in a response as:

```
Foo: bar
```

## Upload a Blob

```
POST /{store-id}
POST /{store-id}/{digest}
```

Uploads a new blob. The request body is the blob content.
Labels are specified as `Flob-<Key>` request headers.
If a blob with the same digest already exists, the server returns `409 Conflict`.

When a digest is provided in the path, it is verified against the computed digest.
If they do not match, the server returns `422 Unprocessable Content`.

Example request:

```http
POST /my-store HTTP/1.1
Content-Type: application/octet-stream
Flob-Foo: bar

<blob content>
```

Example request with digest:

```http
POST /my-store/3b4c... HTTP/1.1
Content-Type: application/octet-stream
Flob-Foo: bar

<blob content>
```

Success response (`201 Created`):

```http
HTTP/1.1 201 Created
Location: /my-store/3b4c...
ETag: "3b4c..."
Content-Length: 1234
Foo: bar
```

## Get Blob Metadata

```
HEAD /{store-id}/{digest}
```

Retrieves blob metadata without downloading its content.
Returns `404 Not Found` if the blob does not exist.

Success response (`200 OK`):

```http
HTTP/1.1 200 OK
ETag: "3b4c..."
Content-Length: 1234
Foo: bar
```

## Download a Blob

```
GET /{store-id}/{digest}
```

Downloads the blob content. Supports partial downloads via `Range` and
standard conditional requests via `If-None-Match` / `If-Match`.
Returns `404 Not Found` if the blob does not exist.

Success response (`200 OK`):

```http
HTTP/1.1 200 OK
ETag: "3b4c..."
Content-Length: 1234
Foo: bar

<blob content>
```

## Update Labels

```
PATCH /{store-id}/{digest}
```

Replaces all labels of a blob. The new label set is taken entirely from the
`Flob-<Key>` request headers — existing labels are discarded and replaced with the provided ones.
The request body must be empty.
Returns `404 Not Found` if the blob does not exist.

Example request:

```http
PATCH /my-store/3b4c... HTTP/1.1
Flob-Foo: baz
```

Success response (`204 No Content`).

## Delete a Blob

```
DELETE /{store-id}/{digest}
```

Removes a blob from the store.
Returns `204 No Content` even if the blob does not exist.

Success response (`204 No Content`).

## Error Responses

All error responses are plain text containing a human-readable message.

For all endpoints, `400 Bad Request` is returned if the store ID or digest does not conform to the expected format.

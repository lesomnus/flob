# flob CLI

## Public read-only serving

The server can expose only selected namespaces and reject all mutations:

```yaml
stores:
  os/public:
    path: /data
server:
  use: os/public
  read_only: true
  allowed_stores: ["_"]
```

`read_only` permits GET and HEAD and returns 405 for other methods before
accessing storage. `allowed_stores` compares decoded namespace IDs and returns
404 for other namespaces, even if they hold the requested digest. By default
the server remains writable and an empty allowlist permits every namespace.
Mount `/data` read-only as well when another process owns publication.

## HTTP read origins and authentication

HTTP stores use an ordered `targets` list. GET and HEAD advance to the next
target only if the transport fails before any HTTP response. Status codes such
as 401, 404 and 503 are terminal. Writes do not fail over.

```yaml
stores:
  http/remote:
    targets:
      - https://primary.example/blobs
      - https://backup.example/blobs
    auth:
      "https://primary.example":
        kind: bearer
        value: "${env:PRIMARY_BLOB_TOKEN}"
      "https://backup.example":
        kind: basic
        value: "${env:BACKUP_BLOB_CREDENTIALS}"
client:
  use: http/remote
```

Authentication keys are origins: scheme, hostname and effective port, without a
path or query. HTTPS defaults to port 443 and HTTP to 80. Public origins omit
their auth entry. Each entry has `kind` and `value`:

- `kind: bearer`: `value` is the token, without the `Bearer ` prefix.
- `kind: basic`: `value` is the unencoded `username:password` pair. Passwords may
  contain colons. In the example, BACKUP_BLOB_CREDENTIALS contains the whole pair.

`value` uses `xli/cfg.Secret`: a literal, `${env:NAME}`, or `${file:/path}`. A
reference must occupy the whole value. For example, a token file can be given
as `value: "${file:/run/secrets/blob-token}"`. Environment references resolve
when the configuration is read. Missing or empty referenced variables fail
configuration. File credentials are rechecked on each request, including after
rotation; unavailable or invalid credentials fail the request without failover.
Unknown kinds and missing values are configuration errors. `flob conf` preserves
references and redacts literal credentials.

Redirects select authentication for their actual origin, so a primary credential
is never inherited by a different origin. Configured auth applies to all paths
at its origin.

The previous singular `target` setting is replaced by `targets`, even for a
single server. Downloads pin the first GET response URL for subsequent range
requests. An interrupted response body is returned to the caller, not retried
or joined to bytes from another server.

## OIDC authentication

An internal publisher can require GitHub Actions job identities instead of a
stored credential. Omit `server.auth` to keep unauthenticated serving. When it
is present, every supported operation requires a Bearer ID token and a matching
grant. Null/empty policies, unknown auth fields, and invalid grants are errors.

```yaml
stores:
  os/published:
    path: /data
server:
  use: os/published
  allowed_stores: ["_"]
  auth:
    providers:
      github:
        kind: oidc
        issuer: https://token.actions.githubusercontent.com
        audience: blob.example.com
    permissions:
      publish:
        stores: ["_"]
        actions: [read, add]
    matches:
      release:
        for: github
        when:
          repository_id: "123456789"
          workflow_ref: acme/assets/.github/workflows/publish.yml@refs/heads/main
          ref: refs/heads/main
        grant: [publish]
```

`when` compares every named claim as an exact string (AND); missing, non-string,
or different values do not match. Separate matches can grant additional named
permissions. Bind GitHub jobs to an approved repository ID and workflow/ref:
any repository can request an ID token for a chosen audience. For reusable
workflows, `job_workflow_ref` can additionally constrain the called workflow.
These claims authenticate jobs, not individual runner machines.

Permissions select decoded namespace IDs exactly; `"*"` selects every namespace.
Actions are `read` (GET/HEAD), `add` (POST), `label` (PATCH), and `erase` (DELETE).
The example permits upload and read of published `_` blobs, with no label changes
or deletion. Publishing into `_` makes data visible to a separate public reader
of the same store root. `read_only` and `allowed_stores` remain additional limits
and cannot be overridden by a grant.

The verifier requires HTTPS discovery/JWKS, checks the discovery issuer and the
token signature, issuer, audience, subject, expiry and issued-at (plus not-before
when supplied). Standard times allow 30 seconds of clock skew. Signing keys are
cached for at most five minutes; unknown IDs can trigger a refresh at most once
per ten seconds. Failed refreshes deny access after the cache expires. Missing
or invalid credentials return 401, insufficient grants 403, and unavailable
provider metadata 503. Rejections happen before opening storage or consuming an
upload body, and responses do not expose credentials or verifier diagnostics.

The scratch image does not include a CA bundle. Mount trusted roots at
`/etc/ssl/certs/ca-certificates.crt`, or configure `SSL_CERT_FILE`/`SSL_CERT_DIR`,
so HTTPS discovery and JWKS requests can verify the provider certificate.

An upload step obtains a fresh, short-lived ID token and sends it directly:

```yaml
permissions:
  contents: read
  id-token: write
steps:
  # Run after producing asset.bin; use an HTTPS endpoint reachable by this job.
  - shell: bash
    run: |
      token=$(curl --fail --silent --show-error \
        -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=blob.example.com" | jq -er .value)
      echo "::add-mask::$token"
      curl --fail --silent --show-error \
        -H "Authorization: Bearer $token" \
        --data-binary @asset.bin https://blob.example.com/_
```

Refresh the ID token before later requests once it expires. This endpoint accepts
OIDC Bearer tokens directly; it does not issue registry-style login tokens.

## Commands

### Argument Conventions

- `STORE_ID` is the store identifier. It is used to specify which store to operate on.
- `DIGEST` is the digest of the blob. It is used to specify which blob to operate on.
	
	The digest can be specified as a 64-character hexadecimal SHA-256 string (with or without the `sha256:` prefix) or a filepath.
	Input is treated as a filepath if it starts with a slash (`/`) or a dot (`.`), or if it is `-` (standard input) and if a filepath is provided, the digest will be calculated from the file content.

- `FILE` is the file path. It is used to specify the file to read from.

	To read from standard input, use `-` as the file path.

### Dump Configs

```bash
flob conf
```

If no config files are found, it will print default config.
```yaml
stores:
  http/local:
    targets: [http://localhost:8080]
  os/cwd:
    path: .flob
server:
  use: mem
  addr: tcp4:0.0.0.0:8080
client:
  use: http/local
otel:
  processors:
    resource/flob:
      attributes:
      - key: service.name
        value: flob
      - key: service.version
        value: v0.0.0-local
  exporters:
    pretty: {}
  providers:
    logger:
      processors:
      - resource/flob
      exporters:
      - pretty
```

### Add a Blob

```sh
flob add [--parallel N] <STORE_ID> <FILE>...
```

```sh
# Add a blob into store "foo" from a file
$ flob add foo ./path/to/file
# Add a blob into store "foo" from standard input
$ echo "Royale with Cheese" | flob add foo -
# Add many, four at a time
$ flob add --parallel 4 foo ./blobs/*.bin
```

It will print the digest of each blob the store now holds, one per line, **in
the order the files were given** — so a caller can pair them with its own list
positionally even when they finish out of order. A file that failed prints
nothing: the digest was computed on the way to failing, and printing it would
claim the store has bytes it does not.
```
4833c026fdec5fe24871c2245b6ea0c392c01057f6c6f4637bcabf8b80e35753
```

Take the whole set in one invocation rather than one process per file. A store's
client, its configuration and its credentials are built once per process, and a
publish that spawns a process per blob pays for all of that thousands of times.
`--parallel` (default 1) then overlaps the round trips within that one process.
Standard input forces sequential: it can only be read once.

A file the store already has is **not a failure**. Content is addressed by
digest, so a blob that is already there is the end state the command asked for,
which is what makes re-running a publish cheap; its digest is printed like any
other. The exit code says which happened:

| exit | meaning |
|------|---------|
| `0`  | at least one blob was written |
| `3`  | every file was already in the store — nothing to do |
| `1`  | something failed |

Every file is attempted even if an earlier one fails, so one run reports
everything that is wrong rather than one problem per re-run.

### Get a Blob

```sh
flob get <STORE_ID> <DIGEST>
```

```sh
# Get a blob from store "foo" with the specified digest and print it to standard output
$ flob get foo 4833c026fdec5fe24871c2245b6ea0c392c01057f6c6f4637bcabf8b80e35753
> Digest: 4833c026fdec5fe24871c2245b6ea0c392c01057f6c6f4637bcabf8b80e35753
> Size: 19
```

It can be used to check if a blob exists in the store by feeding the file content to `flob get` and checking if it returns an error.
```sh
$ echo "Le Big Mac" | flob get foo -
> app exited with error: run command: op: not exist
```

### Open a Blob

```sh
flob open <STORE_ID> <DIGEST>
```

```sh
# Open a blob from store "foo" with the specified digest and print it to standard output
$ flob open foo 4833c026fdec5fe24871c2245ea0c392c01057f6c6f4637bcabf8b80e35753
> Royale with Cheese
```

### Erase a Blob

```sh
flob erase <STORE_ID> <DIGEST>
```

```sh
# Erase a blob from store "foo" with the specified digest
$ flob erase foo 4833c026fdec5fe24871c2245ea0c392c01057f6c6f4637bcabf8b80e35753
```

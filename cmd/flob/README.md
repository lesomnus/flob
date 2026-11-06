# flob CLI

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
    target: http://localhost:8080
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

# logrift

A small, self-hosted log collector and viewer. Applications `POST` structured
JSON logs to an HTTP endpoint; logrift stores them as newline-delimited JSON
files and serves a searchable web UI.

- **Store:** append-only JSONL, one file per UTC day, with retention.
- **Index:** [Bleve](https://github.com/blevesearch/bleve) full-text index,
  persisted on disk. The JSONL files are the source of truth; startup resumes
  from the last indexed byte offset and only reads the tail.
- **Ingest:** `POST /api/logs` accepts a single object, a JSON array, or
  newline-delimited JSON.
- **View:** a built-in web UI with text, level, service and time-range filters
  plus a live mode.

## Run

```sh
make run          # builds ./bin/server and starts it on 127.0.0.1:8787
```

Then open <http://127.0.0.1:8787>. Send some logs:

```sh
curl -s -X POST http://127.0.0.1:8787/api/logs \
  -H 'Content-Type: application/json' \
  -d '[{"level":"error","service":"api","msg":"boom","status":500}]'
```

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `LOGRIFT_ADDR` | `127.0.0.1:8787` | HTTP listen address |
| `LOGRIFT_DATA_DIR` | `./data` | Directory for JSONL files |
| `LOGRIFT_INDEX_DIR` | `<data>/index` | Directory for the persistent index |
| `LOGRIFT_REINDEX` | *(empty)* | Set to `1` to discard and rebuild the index at startup |
| `LOGRIFT_INGEST_TOKEN` | *(empty)* | If set, ingest requires this bearer token |
| `LOGRIFT_RETENTION_DAYS` | `14` | Delete day files older than this (`0` keeps forever) |
| `LOGRIFT_MAX_BODY_KB` | `5120` | Maximum ingest request size in KiB |
| `LOGRIFT_MAX_RESULTS` | `1000` | Maximum search page size |
| `LOGRIFT_PRUNE_INTERVAL_MIN` | `60` | How often retention runs |

## HTTP API

| Method & path | Description |
| --- | --- |
| `POST /api/logs` | Ingest one or many entries. Returns `{"accepted": n}`. |
| `GET /api/search` | Search. Query params: `q`, `level`, `service`, `since`, `until`, `limit`, `offset`. |
| `GET /api/stats` | Totals per level. |
| `GET /healthz` | Liveness check. |

`since`/`until` accept either a duration relative to now (`15m`, `1h`, `7d`) or
an RFC3339 timestamp. Results are returned newest first.

## Entry format

The canonical stored record is:

```json
{"time":"2026-09-27T09:53:03.569Z","level":"error","service":"api","message":"boom","attrs":{"status":500}}
```

The parser is lenient about incoming shapes:

- `level` may be a string (`warn`, `WARNING`, `err`) or a numeric pino level
  (`30` → info).
- `time`, `timestamp`, `ts` or `@timestamp` are accepted; values may be RFC3339
  strings or Unix seconds/millis/micros/nanos.
- `service`/`name`/`app` and `message`/`msg` are accepted as aliases.
- Any other top-level fields are stored under `attrs`.

## Layout

```
cmd/server/main.go        startup, replay, graceful shutdown
internal/config           environment configuration
internal/entry            canonical record + lenient JSON parser
internal/store            JSONL append, replay, rotation, retention
internal/index            Bleve index: add, search, count
internal/server           ingest + query API + embedded web UI
```

## Development

```sh
make check        # gofmt, go vet, go test
```

## Persistence and recovery

The index lives in `LOGRIFT_INDEX_DIR` and survives restarts. Each stored line
is indexed under a stable document ID derived from its file and byte offset, and
the server records the last indexed offset per day file in
`<index>.meta.json`. On startup it seeks to those offsets and indexes only the
tail, so a clean restart is fast regardless of how much history is retained.

Because document IDs are stable, indexing is idempotent: if the process crashes
between writing a line to JSONL and indexing it, or if the meta file is lost,
the next startup re-indexes the affected lines and overwrites them rather than
creating duplicates. Retention prunes both the JSONL files and the matching
index documents.

To force a full rebuild (for example after changing the index mapping or if the
index becomes corrupt), start with `LOGRIFT_REINDEX=1`; the index and its meta
file are removed and rebuilt from the JSONL files.

## Notes and limits

- Single-node only: there is no replication or clustering.
- Ingest persists and indexes synchronously before returning `202`.
- Searching is serialized with a mutex.
- JSONL appends are single writes but not fsynced per line; a crash mid-write
  can leave a partial final line, which is skipped on the next scan. The next
  append could then concatenate onto that partial line, so treat unclean
  shutdowns as a cue to run `LOGRIFT_REINDEX=1`.

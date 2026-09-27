# logrift

A small, self-hosted, multi-project log collector and viewer. Applications
`POST` structured JSON logs with their project's API key; logrift keeps each
project's JSONL files and search index separate and serves a searchable web UI.

- **Projects:** each project has a name, an ingest API key, its own JSONL store
  and its own persistent index. Projects are created and rotated from the UI or
  admin API.
- **Store:** append-only JSONL, one file per UTC day, with retention.
- **Index:** [Bleve](https://github.com/blevesearch/bleve) full-text index,
  persisted on disk. The JSONL files are the source of truth; startup resumes
  from the last indexed byte offset and only reads the tail.
- **Ingest:** `POST /api/logs` accepts a single object, a JSON array, or
  newline-delimited JSON, authenticated by the project's key.
- **View:** an admin-protected web UI with a project selector and text, level,
  service and time-range filters plus a live mode.

## Run

```sh
make run          # builds ./bin/server and starts it on 127.0.0.1:8787
```

The first run generates an admin key and stores it in `./data/admin.key`, then
logs it to stdout. Open <http://127.0.0.1:8787>, enter that key, and create a
project. The project's key is shown once; use it to send logs:

```sh
curl -s -X POST http://127.0.0.1:8787/api/logs \
  -H 'Authorization: Bearer lr_...' \
  -H 'Content-Type: application/json' \
  -d '[{"level":"error","service":"api","msg":"boom","status":500}]'
```

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `LOGRIFT_ADDR` | `127.0.0.1:8787` | HTTP listen address |
| `LOGRIFT_DATA_DIR` | `./data` | Root for the registry, admin key and project data |
| `LOGRIFT_ADMIN_KEY` | stored in `<data>/admin.key` | Admin key for the UI and admin API; required for reads |
| `LOGRIFT_REINDEX` | *(empty)* | Set to `1` to rebuild every project's index at startup |
| `LOGRIFT_RETENTION_DAYS` | `14` | Delete day files older than this (`0` keeps forever) |
| `LOGRIFT_MAX_BODY_KB` | `5120` | Maximum ingest request size in KiB |
| `LOGRIFT_MAX_RESULTS` | `1000` | Maximum search page size |
| `LOGRIFT_PRUNE_INTERVAL_MIN` | `60` | How often retention runs |

If `LOGRIFT_ADMIN_KEY` is not set, logrift generates one on first run and saves
it in `<data>/admin.key` so it survives restarts.

## Projects and API keys

- **Ingest keys** are per project, shown only when created or rotated, and
  stored as SHA-256 hashes. They can only write to their own project.
- **The admin key** protects the web UI and the `/api/projects*` and
  `/api/search` / `/api/stats` endpoints, which can read across projects.
- Send a key as `Authorization: Bearer <key>` or `X-Logrift-Token: <key>`. The
  admin key is also accepted as `X-Logrift-Admin`.

## HTTP API

| Method & path | Auth | Description |
| --- | --- | --- |
| `POST /api/logs` | project key | Ingest one or many entries. |
| `GET /api/search` | admin | Search. Params: `project`, `q`, `level`, `service`, `since`, `until`, `limit`, `offset`. |
| `GET /api/stats` | admin | Totals per level for a project or `all`. |
| `GET /api/projects` | admin | List projects with per-level counts. |
| `POST /api/projects` | admin | Create a project. Body `{"name","description"}`; returns the key once. |
| `POST /api/projects/{name}/rotate` | admin | Issue a new key. |
| `DELETE /api/projects/{name}` | admin | Delete a project and its data. |
| `GET /healthz` | none | Liveness check. |

`project` defaults to `all`, which merges results across projects newest first.
`since`/`until` accept a duration relative to now (`15m`, `1h`, `7d`) or an
RFC3339 timestamp.

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

## Storage layout

```
data/
  admin.key
  projects.json
  projects/
    <name>/
      logs-YYYYMMDD.jsonl
      index/            # persistent Bleve index
      index.meta.json   # indexed byte offsets per day file
```

## Layout

```
cmd/server/main.go        startup, admin key, prune, graceful shutdown
internal/config           environment configuration
internal/entry            canonical record + lenient JSON parser
internal/store            JSONL append, scan, rotation, repair, retention
internal/index            persistent Bleve index: add, search, count, prune
internal/collect          indexed writes + tail catch-up per project
internal/project          project registry + hashed API keys
internal/manager          per-project store/index/collector lifecycle
internal/server           ingest + admin + query API + embedded web UI
```

## Development

```sh
make check        # gofmt, go vet, go test
```

## Persistence and recovery

Each project's index lives in its own directory and survives restarts. Every
stored line is indexed under a stable document ID derived from its file and byte
offset, and the last indexed offset per day file is recorded in
`index.meta.json`. On startup each project seeks to those offsets and indexes
only the tail, so a clean restart is fast regardless of retained history.

Because document IDs are stable, indexing is idempotent: if the process crashes
between writing a line to JSONL and indexing it, or if a meta file is lost, the
next startup re-indexes the affected lines and overwrites them rather than
creating duplicates. On startup a partial trailing line left by an unclean
shutdown is truncated so the next append cannot concatenate onto it. Retention
prunes both the JSONL files and the matching index documents.

To force a full rebuild (for example after changing the index mapping, or if an
index becomes corrupt), start with `LOGRIFT_REINDEX=1`; every project's index
and meta file are removed and rebuilt from its JSONL files.

## Notes and limits

- Single-node only: there is no replication or clustering.
- Ingest persists and indexes synchronously before returning `202`.
- All projects are opened at startup; this suits a handful of projects, not
  thousands.
- Searching is serialized with a mutex per project; `all` queries each project
  in turn and merges.
- JSONL appends are single writes but not fsynced per line.

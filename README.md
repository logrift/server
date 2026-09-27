# logrift

A small, self-hosted, multi-project log collector and viewer. Applications
`POST` structured JSON logs with their project's API key; logrift keeps each
project's JSONL files and search index separate and serves a searchable web UI.

- **Projects:** each project has a name, an ingest API key, its own JSONL store
  and its own persistent index. Projects are created and rotated from the UI or
  admin API.
- **Store:** append-only JSONL, one file per UTC day. Old day files are
  gzip-compressed and archived (see below).
- **Index:** [Bleve](https://github.com/blevesearch/bleve) full-text index,
  persisted on disk. The JSONL files are the source of truth; startup resumes
  from the last indexed byte offset and only reads the tail.
- **Ingest:** `POST /api/logs` accepts a single object, a JSON array, or
  newline-delimited JSON, authenticated by the project's key.
- **View:** an admin-protected web UI with a project selector and text, level,
  service and time-range filters plus a live mode, a per-project storage usage
  overview and a viewer/download for stored (including compressed) logs.

## Run

```sh
make run          # builds ./bin/server and starts it on 127.0.0.1:8787
```

Settings come from a JSON config file; `./logrift.json` is used by default and
created with defaults on first run (`-config <path>` selects another file). The
first run also generates an admin key and stores it in `./data/admin.key`, then
logs it to stdout. Open <http://127.0.0.1:8787>, enter that key, and create a
project. The project's key is shown once; use it to send logs:

```sh
curl -s -X POST http://127.0.0.1:8787/api/logs \
  -H 'Authorization: Bearer lr_...' \
  -H 'Content-Type: application/json' \
  -d '[{"level":"error","service":"api","msg":"boom","status":500}]'
```

## Configuration

All settings live in the config file (`logrift.json`, or the file passed to
`-config`). A missing file is written with the defaults below; absent keys keep
their default value. Settings can also be viewed and edited at runtime from the
**Settings** panel in the web UI (or `GET`/`PATCH /api/settings`); edits are
saved back to the file. `addr`, `data_dir` and `reindex` only take effect after
a restart.

| Key | Default | Purpose |
| --- | --- | --- |
| `addr` | `127.0.0.1:8787` | HTTP listen address |
| `data_dir` | `./data` | Root for the registry, admin key and project data |
| `reindex` | `false` | Rebuild every project's index at startup |
| `compress_after_days` | `14` | Default for new projects: compress and unindex logs older than this (`0` = never) |
| `compress_interval_min` | `60` | How often the compression pass runs (`0` disables the background pass) |
| `max_body_kb` | `5120` | Maximum ingest request size in KiB |
| `max_results` | `1000` | Maximum search page size |

The admin key is not configured in the file: logrift generates one on first run
and saves it in `<data>/admin.key` so it survives restarts.

## Compression and archives

Each project has its own `compress_after_days` setting (seeded from the config
default at creation, editable per project in the Settings panel or via `PATCH
/api/projects/{name}`). Once logs are older than that many days:

- their day file is gzip-compressed to `logs-YYYYMMDD.jsonl.gz` and kept on
  disk forever (nothing is deleted),
- their entries are removed from the search index, so they no longer show up in
  search results.

Compressed days remain readable: the web UI's "Browse stored logs" view lists
every stored day with its size and lets you view or download the raw JSONL for
any date range, compressed or not (`GET /api/projects/{name}/archive`). Set the
setting to `0` to keep everything searchable forever.

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
| `GET /api/settings` | admin | Current settings, the config file path and which settings need a restart. |
| `PATCH /api/settings` | admin | Update settings; persisted to the config file. |
| `GET /api/projects` | admin | List projects with per-level counts and disk usage. |
| `POST /api/projects` | admin | Create a project. Body `{"name","description","compress_after_days"}`; returns the key once. |
| `PATCH /api/projects/{name}` | admin | Update settings. Body `{"compress_after_days":N}` (`0` disables). |
| `POST /api/projects/{name}/rotate` | admin | Issue a new key. |
| `DELETE /api/projects/{name}` | admin | Delete a project and its data. |
| `GET /api/projects/{name}/days` | admin | List stored day files with sizes and compression state. |
| `GET /api/projects/{name}/archive` | admin | Stored log lines for `from`/`to` dates (YYYY-MM-DD). Returns a JSON page (`limit`, `offset`); `raw=1` downloads the plain JSONL. |
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
      logs-YYYYMMDD.jsonl      # live day files
      logs-YYYYMMDD.jsonl.gz   # compressed archive of aged day files
      index/                   # persistent Bleve index
      index.meta.json          # indexed byte offsets per day file
```

## Layout

```
cmd/server/main.go        startup, config, admin key, compression loop, shutdown
internal/config           JSON config file
internal/entry            canonical record + lenient JSON parser
internal/store            JSONL append, scan, rotation, repair, compression
internal/index            persistent Bleve index: add, search, count, delete
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
only the tail, so a clean restart is fast regardless of history size.

Because document IDs are stable, indexing is idempotent: if the process crashes
between writing a line to JSONL and indexing it, or if a meta file is lost, the
next startup re-indexes the affected lines and overwrites them rather than
creating duplicates. On startup a partial trailing line left by an unclean
shutdown is truncated so the next append cannot concatenate onto it. The
compression pass archives aged day files and removes their index documents.

To force a full rebuild (for example after changing the index mapping, or if an
index becomes corrupt), set `"reindex": true` in the config file; every
project's index and meta file are removed and rebuilt from its JSONL files.

## Notes and limits

- Single-node only: there is no replication or clustering.
- Ingest persists and indexes synchronously before returning `202`.
- All projects are opened at startup; this suits a handful of projects, not
  thousands.
- Searching is serialized with a mutex per project; `all` queries each project
  in turn and merges.
- JSONL appends are single writes but not fsynced per line.

# alcyona

A general-purpose personal assistant agent in a single Go binary. Alcyona
learns about each user over time from conversations and completed tasks.
It is primarily driven through a web UI and keeps all state locally in a
single SQLite file.

Status: **skeleton**. The binary builds, serves the placeholder web UI, and
routes `POST /api/chat` to an external OpenAI-compatible model. Conversation
history, per-user memory, tools, and streaming are TODO.

## Design

- Single static binary (Linux + Windows), no runtime dependencies.
- External model: any OpenAI-compatible endpoint (Ollama, a llama.cpp
  server, OpenAI, ...).
- Local state: one SQLite file (users, conversations, messages, memories).
- Web UI: static assets embedded in the binary.

## Layout

```
cmd/alcyona/           entry point: flags, wiring, graceful shutdown
internal/config/       runtime configuration (flags + env)
internal/store/        SQLite persistence + schema
internal/modelclient/  OpenAI-compatible chat client
internal/agent/        the harness: turn loop, memory, tools (TODO)
internal/server/       HTTP server: UI, /health, /api/chat
web/                   embedded web UI assets
```

## Build

```
make build            # bin/alcyona
make build-linux      # bin/alcyona-linux-amd64
make build-windows    # bin/alcyona-windows-amd64.exe
make test
```

## Run

```
ALCYONA_MODEL_BASE_URL=http://localhost:11434/v1 \
ALCYONA_MODEL_NAME=llama3.1 \
./bin/alcyona -addr :8080
```

Then open http://localhost:8080.

## Configuration

| Flag    | Env                    | Default                    | Meaning                          |
|---------|------------------------|----------------------------|----------------------------------|
| `-addr` | `ALCYONA_ADDR`         | `:8080`                    | HTTP listen address              |
| `-data` | `ALCYONA_DATA_DIR`     | `~/.alcyona`               | state directory                  |
|         | `ALCYONA_MODEL_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible base URL     |
|         | `ALCYONA_MODEL_API_KEY`  | (empty)                    | API key for the model            |
|         | `ALCYONA_MODEL_NAME`     | `local-model`              | model identifier                 |

## API (skeleton)

- `GET /health` → `{"status":"ok","time":"..."}`
- `POST /api/chat` → `{"user":"...","message":"..."}` → `{"reply":"..."}`

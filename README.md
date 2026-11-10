# bevin

A general-purpose personal assistant agent in a single Go binary. Bevin
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
cmd/bevin/           entry point: flags, wiring, graceful shutdown
internal/config/       runtime configuration (flags + env)
internal/store/        SQLite persistence + schema
internal/modelclient/  OpenAI-compatible chat client
internal/agent/        the harness: turn loop, memory, tools (TODO)
internal/server/       HTTP server: UI, /health, /api/chat
web/                   embedded web UI assets
```

## Build

```
make build            # bin/bevin
make build-linux      # bin/bevin-linux-amd64
make build-windows    # bin/bevin-windows-amd64.exe
make test
```

## Run

```
BEVIN_MODEL_BASE_URL=http://localhost:11434/v1 \
BEVIN_MODEL_NAME=llama3.1 \
./bin/bevin -addr :8080
```

Then open http://localhost:8080.

## Configuration

| Flag    | Env                    | Default                    | Meaning                          |
|---------|------------------------|----------------------------|----------------------------------|
| `-addr` | `BEVIN_ADDR`         | `:8080`                    | HTTP listen address              |
| `-data` | `BEVIN_DATA_DIR`     | `~/.bevin`               | state directory                  |
|         | `BEVIN_MODEL_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible base URL     |
|         | `BEVIN_MODEL_API_KEY`  | (empty)                    | API key for the model            |
|         | `BEVIN_MODEL_NAME`     | `local-model`              | model identifier                 |

## API (skeleton)

- `GET /health` → `{"status":"ok","time":"..."}`
- `POST /api/chat` → `{"user":"...","message":"..."}` → `{"reply":"..."}`

## License

AGPL-3.0 — see [LICENSE](LICENSE). Bevin is a network service: the Affero
clause means that anyone who modifies it and offers it over a network must
publish those modifications.


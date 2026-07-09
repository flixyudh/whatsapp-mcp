# whatsapp-mcp

A self-hosted MCP (Model Context Protocol) server, written in Go, that gives
an MCP client (Claude, or any other MCP-compatible tool) the ability to send
and read WhatsApp messages.

- **WhatsApp connection**: [whatsmeow](https://github.com/tulir/whatsmeow) —
  the WhatsApp Web multi-device protocol. No official Business API account
  needed, just a phone to scan a QR code with (like linking a device in the
  WhatsApp app).
- **MCP transport**: HTTP/SSE, via [mcp-go](https://github.com/mark3labs/mcp-go),
  so any number of remote MCP clients can connect.
- **Storage**: SQLite only — one file for whatsmeow's session/crypto state,
  one for chat/message history that backs the MCP tools.

## How it works

1. On first run, the server has no WhatsApp session, so it starts a QR
   pairing flow. Scan the code (printed in the terminal **and** served as a
   PNG at `/qr`) with WhatsApp → Linked Devices → Link a Device.
2. Once paired, whatsmeow keeps a persistent multi-device WebSocket
   connection to WhatsApp. Every inbound and outbound message is mirrored
   into a local SQLite history store.
3. WhatsApp also pushes a one-time history sync after pairing, which is
   parsed and backfilled into the same store (best-effort — this is an
   internal WhatsApp payload format, not a public API).
4. MCP clients connect to `/sse` and can call the tools below.

## Tools exposed

| Tool | Description |
|---|---|
| `send_message` | Send a text message to a phone number, JID, or group JID. |
| `list_chats` | List chats by recent activity, with optional name search. |
| `get_chat_history` | Paginated message history for one chat. |
| `search_messages` | Substring search across stored message history. |
| `search_contacts` | Search synced contacts by name or number. |
| `connection_status` | Check whether the WhatsApp session is currently paired. |

A plain REST API mirroring the send/list/status operations is also available
for non-MCP callers (e.g. n8n's HTTP Request node) — see "Using this from
n8n" below.

## Running locally

Requires Go 1.24+ and a C compiler (CGO is enabled — `github.com/mattn/go-sqlite3`
is a cgo binding to SQLite).

```bash
cp .env.example .env
# edit .env if needed, then just:
go run ./cmd/server
```

The server reads `.env` from the working directory automatically (real
environment variables, if set, still take precedence). Point it elsewhere
with `DOTENV_PATH=/path/to/.env`.

Watch the terminal for the QR code, or open `http://localhost:8080/qr` in a
browser. After scanning, the server keeps running and reconnects
automatically; you won't need to re-scan unless you delete
`WHATSAPP_DB_PATH` or get logged out from the phone.

## Running with Docker

```bash
docker compose up --build
```

Same pairing flow — check `docker compose logs -f` for the QR code, or visit
`/qr`. The session and history databases persist in the `wa-data` volume.

For a real deployment, put this behind a reverse proxy that terminates TLS
and forwards to port 8080, and set `PUBLIC_BASE_URL` to your real HTTPS
domain so the `/sse` endpoint advertised to clients is correct. Set
`MCP_AUTH_TOKEN` and require it as a `Authorization: Bearer <token>` header
at the proxy or in-app — this server has no other access control, and
whoever can reach `/sse` can send messages as your WhatsApp account.

## Connecting an MCP client

Point your MCP client at `http://<host>:8080/sse` (SSE transport). If you set
`MCP_AUTH_TOKEN`, configure the client to send
`Authorization: Bearer <token>` on requests.

## Using this from n8n

There are two ways to send a WhatsApp message from an n8n workflow, depending
on whether the decision to send (and what to send) is made by a fixed
workflow step or by an AI Agent:

### Option A — plain REST call (deterministic workflow step)

For a regular node (e.g. after a `Set`, `Code`, or any other node producing a
result) that should always send a message, use an **HTTP Request** node — no
MCP awareness needed:

- Method: `POST`
- URL: `http://<host>:8080/api/v1/messages`
- Headers: `Content-Type: application/json`, plus
  `Authorization: Bearer <token>` if `MCP_AUTH_TOKEN` is set
- Body (JSON):
  ```json
  {
    "to": "6281234567890",
    "message": "={{ $json.resultText }}"
  }
  ```

Response: `{"status":"sent","id":"...","to":"6281234567890@s.whatsapp.net"}`
on success, or `{"status":"error","error":"..."}` with a non-2xx status code
on failure.

Other REST routes, same auth:

| Route | Description |
|---|---|
| `GET /api/v1/status` | `{"paired": true\|false}` — check before sending. |
| `GET /api/v1/chats?q=&limit=&offset=` | List chats as JSON — handy for resolving a chat name to a JID upstream in the workflow. |
| `POST /api/v1/messages` | `{"to": "...", "message": "..."}` → send. |

### Option B — MCP Client Tool node (AI Agent decides)

If an n8n **AI Agent** node should decide dynamically whether/what to send,
add an **MCP Client Tool** node as one of the agent's tools (no server change
needed — n8n speaks MCP/SSE natively):

- Server Transport: `SSE`
- SSE URL: `http://<host>:8080/sse`
- Authentication: Bearer token = your `MCP_AUTH_TOKEN`, if set
- Tools: select `send_message` (and any others you want the agent to use,
  e.g. `list_chats` / `search_contacts` so it can resolve a recipient itself)

The agent can then call `send_message` with `to` and `message` arguments as
part of its reasoning, same as any other tool.

## Project layout

```
cmd/server/main.go            entrypoint: wiring, HTTP server, QR endpoint
internal/config/              env var loading
internal/store/history.go     SQLite chat/message history
internal/whatsapp/client.go   whatsmeow wrapper: pairing, send, event handling
internal/mcpserver/server.go  MCP tool definitions and handlers
```

## First-time setup: fetching dependencies

```bash
go mod tidy
```

This resolves and downloads everything, including `go.mau.fi/whatsmeow`
directly from its real module path (no pinning/mirroring needed on a normal
network).

## Troubleshooting: "Client outdated (405) connect failure"

WhatsApp periodically raises the minimum client version it accepts, and
rejects older connections with this error. Two layers of defense against it
here:

1. `whatsapp.New()` calls `whatsmeow.GetLatestVersion()` at startup and
   applies it via `store.SetWAVersion()`, so an outdated *build* of this
   server still uses a current client version at runtime. Check the logs for
   `using WhatsApp web client version ...` to confirm this ran; if it logs a
   warning instead (network issue reaching web.whatsapp.com), it silently
   falls back to whatsmeow's built-in default, which is what triggers this
   error.
2. If it still happens, the whatsmeow *library* itself is too old for
   WhatsApp's current protocol expectations (rare, but happens after long
   gaps). Fix with:
   ```bash
   go get -u go.mau.fi/whatsmeow@latest
   go mod tidy
   ```

## Notes on dependencies

No `replace` directives are needed for a normal build — plain
`go get -u go.mau.fi/whatsmeow@latest` tracks upstream directly.

## Limitations / things to know

- This uses WhatsApp's unofficial multi-device protocol (the same one
  WhatsApp Web uses), not the official Business Cloud API. It's free and
  needs no business verification, but it's not officially sanctioned —
  don't use it for spam or bulk messaging, that's a fast way to get the
  linked account banned.
- History sync parsing is best-effort; WhatsApp's internal sync payload
  format isn't publicly documented and can change.
- One WhatsApp account per running server instance. If you need several
  accounts, run several instances with separate `WHATSAPP_DB_PATH` /
  `HISTORY_DB_PATH` / ports.
- Media messages are recorded with type + caption only; the binary media
  itself isn't downloaded or stored. Say if you want that added — it's a
  straightforward extension (whatsmeow exposes `client.Download(...)`).
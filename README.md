# Mail Gateway

Standalone SMTP gateway for Personal Cloud Server (PCS) deployments. This service receives emails from containerized applications (Vaultwarden, Nextcloud, etc.) and forwards them over HTTPS to a relay backend (the orchestrator email API) for delivery via SendGrid.

## Architecture

```
App (Vaultwarden, Nextcloud, etc.)
     │
     │ SMTP (port 587)
     ↓
smtp container (this service)
     │
     │ HTTPS API call
     ↓
Relay backend (orchestrator)
     │
     ↓
SendGrid → Recipient
```

## Features

- **SMTP Relay**: Accepts SMTP connections on port 587
- **Email Parsing**: Full RFC-compliant MIME email parsing with inline image support
- **API Forwarding**: Forwards emails to the relay backend with JWT authentication
- **Network Isolation**: Runs in private Docker network (pcs)
- **Multi-platform**: Supports linux/amd64 and linux/arm64
- **Lightweight**: ~20MB Alpine-based Docker image

## Configuration

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RELAY_CREDENTIAL` | Yes | - | The credential the relay verifies. **Explicit mode:** an opaque bearer token (a JWT on Yundera). **Provider mode (nsl):** the PCS provider string `backend_url,userid,signature` — the gateway derives the endpoint and forwards `userid:signature`. |
| `RELAY_ENDPOINT_URL` | Conditional | - | Base URL of the relay backend's email API (gateway POSTs to `{url}/email/send`). Required in explicit mode; **optional in provider mode**, where it defaults to `{backend_url}/router/api`. |
| `SMTP_PORT` | No | `587` | SMTP listening port |
| `STATS_FILE` | No | `/data/stats.json` | Delivery statistics file (see [Delivery statistics](#delivery-statistics)). If it cannot be written, statistics are kept in memory only. |

> **Provider mode** is detected automatically: a `RELAY_CREDENTIAL` with exactly three comma-separated fields (`backend_url,userid,signature`) is treated as a provider string. A JWT contains no commas, so it always uses explicit mode.

### Docker Compose Example

```yaml
services:
  smtp:
    image: ghcr.io/yundera/mail-gateway:1.1.0
    container_name: smtp
    hostname: smtp
    restart: unless-stopped
    environment:
      RELAY_CREDENTIAL: "${RELAY_CREDENTIAL}"
      RELAY_ENDPOINT_URL: "${RELAY_ENDPOINT_URL}"
      SMTP_PORT: "587"
    expose:
      - "587"
    volumes:
      - smtp-data:/data   # keeps delivery statistics across recreation
    networks:
      - pcs

volumes:
  smtp-data:

networks:
  pcs:
    name: pcs
```

Use a **named volume** for `/data`: the image creates `/data` owned by the unprivileged `smtp` user (uid 1000) and
Docker copies that ownership into a new named volume. A bind mount comes up root-owned, so the gateway cannot write it
and falls back to in-memory statistics.

## Delivery behaviour

- **Every recipient gets a copy.** The relay API takes one recipient per call, so each distinct envelope recipient
  (To, Cc and Bcc alike) is sent its own copy, addressed to them alone.
- **The sender is enforced by the relay**: `<app>.<user-domain>@<server-domain>`. Only the local part of the SMTP
  `MAIL FROM` is used, as the app name (lowercased, `[a-z0-9-]`, 20 chars max).
- **SMTP replies:**
  - `250` when the relay accepted every copy. That includes a relay with no delivery provider configured, which answers
    OK with `skipped: true`; the statistics record those as `skipped`, not `sent`.
  - `451` (temporary, the app should retry) when a copy was rate-limited (HTTP 429) or the relay could not be reached.
  - `554` (permanent) when the relay refused a copy with any other error status. This outranks a `451`.
- If one copy of a multi-recipient email fails, the whole transaction fails. An app that retries may then deliver a
  second copy to the recipients that already got one.

## Delivery statistics

Each delivery attempt (one per recipient) is recorded in `STATS_FILE`: the last 200 events (time, app, enforced
sender, recipient, outcome, error), plus hourly per-app counters kept for 30 days. **Subjects and bodies are never
stored.** The file is replaced atomically after every event.

Read the summary with the `stats` subcommand. It reads the file, so it needs no port and no relay configuration:

```bash
docker exec smtp /app/mail-gateway stats
```

```json
{
  "version": "1.1.0",
  "since": "2026-09-30T10:00:00Z",
  "retentionDays": 30,
  "totals": {
    "h24": { "sent": 3, "failed": 0, "skipped": 0, "rateLimited": 0 },
    "d7":  { "sent": 14, "failed": 1, "skipped": 0, "rateLimited": 0 }
  },
  "apps": [
    { "app": "vaultwarden", "from": "vaultwarden.alice@nsl.sh", "last": "2026-09-30T11:58:02Z",
      "sent": 9, "failed": 1, "skipped": 0, "rateLimited": 0 }
  ],
  "recent": [
    { "time": "2026-09-30T11:58:02Z", "app": "vaultwarden", "from": "vaultwarden.alice@nsl.sh",
      "to": "bob@example.com", "status": "sent" }
  ],
  "persistent": true
}
```

- `status` is one of `sent`, `skipped` (accepted by a relay with no delivery provider), `failed` or `rate_limited`.
- `apps` covers the whole retention window, most recently active first. `recent` holds the last 20 events, newest first.
- The totals count whole hours, so `h24` can include up to 59 extra minutes.
- `persistent: false` with empty lists means the file does not exist: the server could not write it, or no server has run
  with this `STATS_FILE`.

## App Configuration

Apps running in the same Docker network can connect to the SMTP service using the hostname `smtp`.

### Vaultwarden Example

```env
SMTP_HOST=smtp
SMTP_PORT=587
SMTP_FROM=vaultwarden@yourdomain.com
SMTP_SECURITY=off  # No TLS needed within Docker network
```

### Nextcloud Example

In Nextcloud admin settings:
- **Server address**: smtp
- **Port**: 587
- **Encryption**: None/STARTTLS

## Security

- **Network Isolation**: Service only accepts connections from the private `pcs` Docker network
- **No Public Exposure**: SMTP port is NOT exposed to the internet
- **Relay authentication**: every API call carries `RELAY_CREDENTIAL` as a bearer token (a JWT, or `userid:signature` in provider mode)
- **Rate Limiting**: the relay enforces rate limits (100 emails/hour per user); the gateway answers `451` so apps retry

## Development

### Build Locally

```bash
docker build -t mail-gateway .
```

### Run Locally

```bash
docker run --rm \
  -e RELAY_CREDENTIAL="your-credential" \
  -e RELAY_ENDPOINT_URL="https://your-relay.example.com/service/pcs" \
  -p 587:587 \
  mail-gateway
```

### Test Email

```bash
# Send a test email using telnet
telnet localhost 587
> EHLO test
> MAIL FROM:<test@example.com>
> RCPT TO:<recipient@example.com>
> DATA
> Subject: Test Email
>
> This is a test email body.
> .
> QUIT
```

## Deployment

This service is automatically deployed to PCS instances via the `docker-compose.yml` template shipped with the mesh-router installer.

### GitHub Actions

The service is automatically built and published to GitHub Container Registry on:
- Push to `main` branch → `latest` tag
- Version tags (e.g., `v1.0.0`) → version-specific tags

## Troubleshooting

### Service won't start

Check logs:
```bash
docker logs smtp
```

Common issues:
- Missing `RELAY_CREDENTIAL` environment variable
- Port 587 already in use
- Network connectivity to orchestrator

### Emails not being sent

1. Check SMTP service logs: `docker logs smtp`
2. Verify orchestrator URL is correct
3. Check JWT token is valid
4. Verify app is configured to use `smtp` as hostname
5. Check orchestrator logs for API errors

## License

Copyright Yundera Team

# Remote web access

Use Wingman from a phone or another computer while the agent runs in your
workspace. A relay connects your browser to the workspace through an outbound
connection, so the workspace does not need a public inbound port.

You need a relay host, an HTTPS gateway for it, and a shared registration token.

## Start the relay

On the relay host:

```bash
WINGMAN_RELAY_TOKEN="your-secret" wingman relay --port 8080
```

The relay serves HTTP on port 8080 by default. Put it behind an HTTPS gateway
at a dedicated origin such as `https://relay.example.com`. Keep its HTTP port
on the gateway's private network.

Configure the gateway to:

- Route the entire origin to the relay, without a URL path prefix.
- Preserve `Host` and set `X-Forwarded-Proto: https`.
- Forward WebSocket upgrades and streaming responses without buffering.
- Allow long-lived WebSocket connections.

The gateway handles TLS and certificates. Plain HTTP relay URLs are accepted
only for loopback development.

## Connect your workspace

In your project directory, use the same token:

```bash
WINGMAN_REMOTE_TOKEN="your-secret" wingman server --remote https://relay.example.com
```

Open the pairing link printed by Wingman, or scan its QR code on your phone.
Local and remote browsers share the running sessions. On a phone, use the
header to switch agents, open the session list, or start a new session.

| Setting | Environment variable | Flag |
|---------|----------------------|------|
| Relay HTTP port | — | `wingman relay --port 8080` |
| Registration token on the relay | `WINGMAN_RELAY_TOKEN` | `wingman relay --token TOKEN` |
| Relay URL for the workspace | `WINGMAN_REMOTE_URL` | `wingman server --remote URL` |
| Registration token on the workspace | `WINGMAN_REMOTE_TOKEN` | `wingman server --remote-token TOKEN` |

Flags override environment variables. These settings are read at startup.

## Pairing and reconnecting

The pairing link works while that workspace server is running. A temporary
connection loss reconnects automatically; the agent keeps working, and the
browser recovers the current transcript. Restarting the workspace server creates
a new pairing link, so pair again after a restart.

One browser can be paired with one workspace per relay origin. Pairing another
workspace changes the selection for that origin. Reload other tabs to use the
new selection. Clear the relay site's browser data to remove its pairing.

The link grants access to files, terminals, and the other web UI features.
Keep it and the registration token private. Use a trusted gateway and relay:
both can see the traffic and pairing credentials.

See [sessions and recovery](sessions.md) for saved queues, drafts, and interrupted sends.

## Run the relay in Docker

Build from the repository root:

```bash
docker build -f docker/relay/Dockerfile -t wingman-relay .
export WINGMAN_RELAY_TOKEN="your-secret"
docker run --rm --network your-gateway-network \
  -e WINGMAN_RELAY_TOKEN wingman-relay --port 8080
```

Connect the HTTPS gateway to the container's port 8080. The relay image runs as
a non-root user and needs no certificate storage or volumes. Run one relay
instance per origin; relay connections are held in memory.

## Troubleshooting

- **No pairing link:** check that both sides use the same registration token
  and the workspace can reach the relay's public HTTPS URL.
- **Chat disconnects or stops streaming:** check WebSocket forwarding, response
  buffering, and the gateway's connection timeouts.
- **Workspace offline or pairing expired:** confirm the workspace server is
  running and open its latest pairing link.

# API documentation

The API has multiple REST endpoints and a Websocket endpoint.

## REST

The REST API documentation is available as OpenAPI specification: [api-spec.yml](/api-spec.yml).

## Spotify Web API Passthrough

The `/web-api/` endpoint is a transparent proxy to the [Spotify Web API](https://developer.spotify.com/documentation/web-api) (`https://api.spotify.com/`). Any path after `/web-api/` is forwarded to Spotify with authentication handled automatically.

Example: `GET /web-api/v1/me/playlists?limit=50` proxies to `https://api.spotify.com/v1/me/playlists?limit=50`.

The response from Spotify is passed through transparently (status code, headers, body). All HTTP methods are supported.

### Rate Limiting

By default, this endpoint shares the daemon's internal token with playback operations, which can lead to Spotify rate limiting (HTTP 429). To avoid this, configure `server.dev_api` with your own Spotify Developer API credentials (see below).

## Token Endpoint

`POST /token` returns a fresh Spotify access token:

```json
{"token": "BQD..."}
```

## Developer API Token

To use a separate rate limit bucket for `/web-api/` requests, configure your own Spotify Developer API credentials.

### Setup

1. Create an app at https://developer.spotify.com/dashboard
2. Add a redirect URI pointing to your go-librespot `/devapi/callback` endpoint. Spotify requires HTTPS, so this should go through your reverse proxy (e.g. `https://example.com/devapi/callback`).
3. Add the credentials to your config:

```yaml
server:
  enabled: true
  port: 3678
  dev_api:
    client_id: "your-client-id"
    client_secret: "your-client-secret"
    redirect_uri: "https://example.com/devapi/callback"
```

4. Start go-librespot — if not yet authorized, the auth URL is logged at startup
5. Open `/devapi/authorize` in your browser (redirects to Spotify) or use the URL from the log
6. Authorize the app on Spotify's page — you'll be redirected back automatically
7. The refresh token is persisted in `state.json` and survives restarts

### Configuration

| Field | Required | Description |
|-------|----------|-------------|
| `client_id` | Yes | Client ID from your Spotify Developer Dashboard |
| `client_secret` | No | Client Secret (optional for PKCE-only apps) |
| `redirect_uri` | Yes | HTTPS callback URL matching your Spotify app's redirect URI |
| `scopes` | No | OAuth2 scopes to request (defaults to common read/write scopes) |

### Endpoints

- `GET /devapi/status` - Returns `{"configured": bool, "authorized": bool}`
- `GET /devapi/authorize` - Redirects to Spotify's OAuth2 authorization page
- `GET /devapi/callback` - OAuth2 callback (handled automatically by the browser redirect)

### Behavior

When authorized, all `/web-api/` requests automatically use the dev API token. If the dev API token fails (e.g. revoked), it falls back to the internal token. The dev API works independently of the Spotify Connect session, so `/web-api/` is available even with no active user (zeroconf mode).

## Websocket

The websocket endpoint is available at `/events`. The following events are emitted:

- `active`: The device has become active
- `inactive`: The device has become inactive
- `metadata`: A new track was loaded, the following metadata is available:
    - `context_uri`: The context URI 
    - `uri`: Track URI
    - `name`: Track name
    - `artist_names`: List of track artist names
    - `album_name`: Track album name
    - `album_cover_url`: Track album cover image URL
    - `position`: Track position in milliseconds
    - `duration`: Track duration in milliseconds
- `will_play`: The player is about to play the specified track
    - `context_uri`: The context URI
    - `uri`: The track URI
    - `play_origin`: Who started the playback
- `playing`: The current track is playing
    - `context_uri`: The context URI
    - `uri`: The track URI
    - `resume`: Was this resumed from paused playback?
    - `play_origin`: Who started the playback
- `not_playing`: The current track has finished playing
    - `context_uri`: The context URI
    - `uri`: The track URI
    - `play_origin`: Who started the playback
- `paused`: The current track is paused
    - `context_uri`: The context URI
    - `uri`: The track URI
    - `play_origin`: Who started the playback
- `stopped`: The current context is empty, nothing more to play
    - `play_origin`: Who started the playback
- `seek`: The current track was seeked, the following data is provided:
    - `context_uri`: The context URI
    - `uri`: The track URI
    - `position`: Track position in milliseconds
    - `duration`: Track duration in milliseconds
    - `play_origin`: Who started the playback
- `volume`: The player volume changed, the following data is provided:
    - `value`: The volume, ranging from 0 to max
    - `max`: The max volume value
- `shuffle_context`: The player shuffling context setting changed
    - `value`: Whether shuffling context is enabled
- `repeat_context`: The player repeating context setting changed
    - `value`: Whether repeating context is enabled
- `repeat_track`: The player repeating track setting changed
    - `value`: Whether repeating track is enabled

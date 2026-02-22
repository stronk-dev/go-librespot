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

## Native Metadata

These endpoints use Spotify's internal APIs and require an active session (return `204 No Content` otherwise). Owner display name enrichment uses internal context/profile endpoints when available.

All `{id}` parameters are base62 Spotify IDs matching `[A-Za-z0-9]{21,22}` (for example `spotify:track:4uLU6hMCjMI75M1A2tKUQC` -> `4uLU6hMCjMI75M1A2tKUQC`). Invalid IDs return `400 Bad Request`.

All metadata endpoints accept an optional `?image_size=small|default|large|xlarge` query parameter to override the configured image size.
Image URL fields (`album_cover_url`, `cover_url`, `portrait_url`, `image_url`) are usually local daemon paths (`/image/{hexid}`), so frontends can avoid direct Spotify CDN requests.

### Track

`GET /metadata/track/{id}` returns track metadata:

```json
{
  "uri": "spotify:track:...",
  "name": "Track Name",
  "artists": [{"uri": "spotify:artist:...", "name": "Artist Name"}],
  "album": {"uri": "spotify:album:...", "name": "Album Name"},
  "album_cover_url": "/image/ab67706c0000da8429b049a771662fae7b917d25",
  "duration": 210000,
  "track_number": 1,
  "disc_number": 1,
  "popularity": 75,
  "explicit": false,
  "release_date": "2024-01-15"
}
```

### Album

`GET /metadata/album/{id}` returns album metadata with full track listing:

```json
{
  "uri": "spotify:album:...",
  "name": "Album Name",
  "artists": [{"uri": "spotify:artist:...", "name": "Artist Name"}],
  "type": "album",
  "label": "Record Label",
  "release_date": "2024-01-15",
  "cover_url": "/image/ab67706c0000da8429b049a771662fae7b917d25",
  "popularity": 80,
  "total_tracks": 12,
  "tracks": [...]
}
```

### Artist

`GET /metadata/artist/{id}` returns artist metadata with top tracks, discography, and related artists:

```json
{
  "uri": "spotify:artist:...",
  "name": "Artist Name",
  "portrait_url": "/image/ab67706c0000da8429b049a771662fae7b917d25",
  "popularity": 85,
  "biography": "Artist biography text...",
  "top_tracks": [...],
  "albums": [{"uri": "spotify:album:...", "name": "Album"}],
  "singles": [{"uri": "spotify:album:...", "name": "Single"}],
  "related": [{"uri": "spotify:artist:...", "name": "Related Artist"}]
}
```

### Show

`GET /metadata/show/{id}` returns podcast/show metadata with episode listing.

### Episode

`GET /metadata/episode/{id}` returns podcast episode metadata.

### Playlist

`GET /metadata/playlist/{id}` returns playlist metadata and track URIs. Supports `?limit=N&offset=M` for pagination.
`limit` must be `>= 1` and `offset` must be `>= 0`; invalid values return `400 Bad Request`.
When a playlist has no explicit cover, `image_url` points to `/metadata/playlist/{id}/image?size={60|300|640}` (generated 2x2 mosaic).
`owner_display_name` is provided when resolvable; otherwise it is omitted and `owner_username` remains the stable fallback.

```json
{
  "uri": "spotify:playlist:...",
  "name": "My Playlist",
  "description": "Playlist description",
  "owner_username": "username",
  "owner_display_name": "Display Name",
  "collaborative": false,
  "image_url": "/image/ab67706c0000da8429b049a771662fae7b917d25",
  "total_tracks": 50,
  "items": [
    {"uri": "spotify:track:...", "added_by": "username", "added_at": 1705315200}
  ]
}
```

`GET /metadata/playlist/{id}/image?size=60|300|640` returns a generated JPEG mosaic cover for playlists without explicit artwork.
The endpoint returns cache headers (`Cache-Control`, `ETag`) and supports conditional requests with `If-None-Match` (`304 Not Modified`).
Both playlist metadata and image endpoints may return `403` (forbidden) or `429` (upstream rate limits).

### Image Proxy

`GET /image/{hexid}` returns image bytes for a 40-char Spotify image ID (hex).
This endpoint backs metadata image URLs and serves cached content from the daemon when available.
It returns cache headers (`Cache-Control`, `ETag`) and supports `If-None-Match` (`304 Not Modified`).

### Rootlist (User's Playlists)

`GET /metadata/rootlist` returns the current user's playlist collection.
Pagination is supported via `?limit=N&offset=M`.
When pagination params are provided, the response includes `total`, `offset`, and `limit`.

```json
{
  "total": 123,
  "offset": 0,
  "limit": 50,
  "playlists": [
    {"uri": "spotify:playlist:...", "name": "My Playlist"},
    {"uri": "spotify:playlist:...", "name": "Another Playlist"}
  ]
}
```

### Queue

`GET /player/queue` returns the current playback queue:

```json
{
  "current": {
    "uri": "spotify:track:...",
    "name": "Current Track",
    "provider": "context",
    "track": {
      "uri": "spotify:track:...",
      "name": "Current Track",
      "artists": [{"uri": "spotify:artist:...", "name": "Artist"}],
      "album": {"uri": "spotify:album:...", "name": "Album"},
      "album_cover_url": "/image/ab67706c0000da8429b049a771662fae7b917d25",
      "duration": 210000
    }
  },
  "prev_tracks": [...],
  "next_tracks": [...]
}
```

Queue entries are enriched inline when resolvable:
- Track URIs include `track` metadata (same shape as `/metadata/track/{id}`)
- Episode URIs include `episode` metadata (same shape as `/metadata/episode/{id}`)

### Context Resolver

`GET /context/{uri}` resolves any Spotify URI to its track list. Works with playlists, albums, artist URIs, stations, etc.
If the URI is URL-escaped in the path, it is decoded before resolve. Invalid escaping returns `400 Bad Request`.

Example: `GET /context/spotify:album:4uLU6hMCjMI75M1A2tKUQC`

### Collection (Liked Songs)

`GET /metadata/collection` returns the current user's Liked Songs:

```json
{
  "items": [
    {"uri": "spotify:track:..."},
    {"uri": "spotify:track:..."}
  ]
}
```

### Radio

`POST /player/radio` starts radio/station playback from a seed URI:

```json
{"seed_uri": "spotify:track:4uLU6hMCjMI75M1A2tKUQC"}
```

Generates an autoplay station based on the seed and starts playback immediately. Works with track, artist, album, and playlist URIs.

## Websocket

The websocket endpoint is available at `/events`. The following events are emitted:

- `active`: The device has become active
- `inactive`: The device has become inactive
- `metadata`: A new track was loaded, the following metadata is available:
    - `uri`: Track URI
    - `name`: Track name
    - `artist_names`: List of track artist names
    - `album_name`: Track album name
    - `album_cover_url`: Track album cover image URL (usually `/image/{hexid}`)
    - `album_uri`: Album URI (for browsing)
    - `artists`: Structured artist list with URIs: `[{"uri": "spotify:artist:...", "name": "..."}]`
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
- `queue`: The playback queue changed
    - `prev_tracks`: List of previous tracks (each with `uri`, `name`, `provider`)
    - `next_tracks`: List of upcoming tracks (each with `uri`, `name`, `provider`)
- `context`: The playback context changed
    - `context_uri`: The new context URI

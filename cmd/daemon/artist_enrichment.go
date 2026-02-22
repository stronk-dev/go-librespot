package main

import (
	"context"
	"strings"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

func trackNeedsEnrichment(track *ApiResponseTrackFull) bool {
	if track == nil {
		return false
	}

	if strings.TrimSpace(track.Name) == "" || track.Duration <= 0 || len(track.Artists) == 0 {
		return true
	}
	if track.Album != nil && strings.TrimSpace(track.Album.Name) == "" {
		return true
	}

	return false
}

func refNeedsNameEnrichment(ref *ApiResponseRef) bool {
	return ref != nil && strings.TrimSpace(ref.Name) == ""
}

type artistEnrichmentCache struct {
	tracks  map[string]ApiResponseTrackFull
	albums  map[string]ApiResponseRef
	artists map[string]ApiResponseRef
}

func newArtistEnrichmentCache() *artistEnrichmentCache {
	return &artistEnrichmentCache{
		tracks:  make(map[string]ApiResponseTrackFull),
		albums:  make(map[string]ApiResponseRef),
		artists: make(map[string]ApiResponseRef),
	}
}

func (p *AppPlayer) enrichArtistResponse(ctx context.Context, resp *ApiResponseArtistFull, imageSize string) {
	if resp == nil {
		return
	}

	cache := newArtistEnrichmentCache()
	for i := range resp.TopTracks {
		p.enrichArtistTrack(ctx, &resp.TopTracks[i], imageSize, cache)
	}
	for i := range resp.Albums {
		p.enrichAlbumRef(ctx, &resp.Albums[i], cache)
	}
	for i := range resp.Singles {
		p.enrichAlbumRef(ctx, &resp.Singles[i], cache)
	}
	for i := range resp.Related {
		p.enrichArtistRef(ctx, &resp.Related[i], cache)
	}
}

func (p *AppPlayer) enrichArtistTrack(ctx context.Context, track *ApiResponseTrackFull, imageSize string, cache *artistEnrichmentCache) {
	if !trackNeedsEnrichment(track) || track == nil || cache == nil {
		return
	}

	if cached, ok := cache.tracks[track.Uri]; ok {
		*track = cached
		return
	}

	trackID, err := librespot.SpotifyIdFromUri(track.Uri)
	if err != nil || trackID == nil || trackID.Type() != librespot.SpotifyIdTypeTrack {
		return
	}

	var meta metadatapb.Track
	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *trackID, extmetadatapb.ExtensionKind_TRACK_V4, &meta); err != nil {
		p.app.log.WithError(err).WithField("uri", track.Uri).Debug("failed enriching artist top track metadata")
		return
	}

	converted := p.convertTrackProto(&meta, imageSize)
	if converted == nil {
		return
	}

	cache.tracks[track.Uri] = *converted
	*track = *converted
}

func (p *AppPlayer) enrichAlbumRef(ctx context.Context, ref *ApiResponseRef, cache *artistEnrichmentCache) {
	if !refNeedsNameEnrichment(ref) || ref == nil || cache == nil {
		return
	}

	if cached, ok := cache.albums[ref.Uri]; ok {
		ref.Name = cached.Name
		return
	}

	albumID, err := librespot.SpotifyIdFromUri(ref.Uri)
	if err != nil || albumID == nil || albumID.Type() != librespot.SpotifyIdTypeAlbum {
		return
	}

	var meta metadatapb.Album
	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *albumID, extmetadatapb.ExtensionKind_ALBUM_V4, &meta); err != nil {
		p.app.log.WithError(err).WithField("uri", ref.Uri).Debug("failed enriching artist album reference metadata")
		return
	}

	enriched := albumRef(&meta)
	cache.albums[ref.Uri] = enriched
	ref.Name = enriched.Name
}

func (p *AppPlayer) enrichArtistRef(ctx context.Context, ref *ApiResponseRef, cache *artistEnrichmentCache) {
	if !refNeedsNameEnrichment(ref) || ref == nil || cache == nil {
		return
	}

	if cached, ok := cache.artists[ref.Uri]; ok {
		ref.Name = cached.Name
		return
	}

	artistID, err := librespot.SpotifyIdFromUri(ref.Uri)
	if err != nil || artistID == nil || artistID.Type() != librespot.SpotifyIdTypeArtist {
		return
	}

	var meta metadatapb.Artist
	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *artistID, extmetadatapb.ExtensionKind_ARTIST_V4, &meta); err != nil {
		p.app.log.WithError(err).WithField("uri", ref.Uri).Debug("failed enriching related artist reference metadata")
		return
	}

	enriched := artistRef(&meta)
	cache.artists[ref.Uri] = enriched
	ref.Name = enriched.Name
}

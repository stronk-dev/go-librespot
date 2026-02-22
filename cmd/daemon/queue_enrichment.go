package main

import (
	"context"
	"strings"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

type queueEnrichmentCache struct {
	tracks   map[string]*ApiResponseTrackFull
	episodes map[string]*ApiResponseEpisodeFull
}

func newQueueEnrichmentCache() *queueEnrichmentCache {
	return &queueEnrichmentCache{
		tracks:   make(map[string]*ApiResponseTrackFull),
		episodes: make(map[string]*ApiResponseEpisodeFull),
	}
}

func (p *AppPlayer) enrichQueueTrack(ctx context.Context, item ApiResponseQueueTrack, imageSize string, cache *queueEnrichmentCache) ApiResponseQueueTrack {
	if p == nil || p.sess == nil || cache == nil {
		return item
	}

	uri := strings.TrimSpace(item.Uri)
	if uri == "" {
		return item
	}

	if track, ok := cache.tracks[uri]; ok {
		if track != nil {
			item.Track = track
			if item.Name == "" {
				item.Name = track.Name
			}
		}
		return item
	}
	if episode, ok := cache.episodes[uri]; ok {
		if episode != nil {
			item.Episode = episode
			if item.Name == "" {
				item.Name = episode.Name
			}
		}
		return item
	}

	spotId, err := librespot.SpotifyIdFromUri(uri)
	if err != nil || spotId == nil {
		return item
	}

	switch spotId.Type() {
	case librespot.SpotifyIdTypeTrack:
		var trackMeta metadatapb.Track
		if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *spotId, extmetadatapb.ExtensionKind_TRACK_V4, &trackMeta); err != nil {
			p.app.log.WithError(err).WithField("uri", uri).Debug("failed enriching queue track metadata")
			cache.tracks[uri] = nil
			return item
		}

		track := p.convertTrackProto(&trackMeta, imageSize)
		cache.tracks[uri] = track
		if track != nil {
			item.Track = track
			if item.Name == "" {
				item.Name = track.Name
			}
		}

	case librespot.SpotifyIdTypeEpisode:
		var episodeMeta metadatapb.Episode
		if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *spotId, extmetadatapb.ExtensionKind_EPISODE_V4, &episodeMeta); err != nil {
			p.app.log.WithError(err).WithField("uri", uri).Debug("failed enriching queue episode metadata")
			cache.episodes[uri] = nil
			return item
		}

		episode := p.convertEpisodeProto(&episodeMeta, imageSize)
		cache.episodes[uri] = episode
		if episode != nil {
			item.Episode = episode
			if item.Name == "" {
				item.Name = episode.Name
			}
		}
	}

	return item
}

func (p *AppPlayer) enrichQueueTracks(ctx context.Context, items []ApiResponseQueueTrack, imageSize string, cache *queueEnrichmentCache) []ApiResponseQueueTrack {
	if len(items) == 0 {
		return []ApiResponseQueueTrack{}
	}

	out := make([]ApiResponseQueueTrack, 0, len(items))
	for _, item := range items {
		out = append(out, p.enrichQueueTrack(ctx, item, imageSize, cache))
	}

	return out
}

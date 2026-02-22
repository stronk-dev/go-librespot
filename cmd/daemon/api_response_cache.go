package main

import (
	"fmt"
	"strings"
	"time"
)

type apiResponseCacheEntry struct {
	Value    any
	CachedAt time.Time
	TTL      time.Duration
}

const (
	apiResponseCacheMaxEntries        = 4096
	apiResponseCacheTTLStaticMetadata = 6 * time.Hour
	apiResponseCacheTTLPlaylist       = 30 * time.Second
)

func normalizeImageSizeCacheKey(requested, fallback string) string {
	size := strings.ToLower(strings.TrimSpace(requested))
	if size == "" {
		size = strings.ToLower(strings.TrimSpace(fallback))
	}
	if size == "" {
		size = "default"
	}
	return size
}

func (p *AppPlayer) apiResponseCacheDescriptor(req ApiRequest) (string, time.Duration, bool) {
	switch req.Type {
	case ApiRequestTypeGetTrack, ApiRequestTypeGetAlbum, ApiRequestTypeGetArtist, ApiRequestTypeGetShow, ApiRequestTypeGetEpisode:
		data, ok := req.Data.(ApiRequestDataGetMetadata)
		if !ok {
			return "", 0, false
		}

		imageSize := normalizeImageSizeCacheKey(data.ImageSize, p.app.cfg.Server.ImageSize)
		return fmt.Sprintf("%s:%s:%s", req.Type, data.Id, imageSize), apiResponseCacheTTLStaticMetadata, true
	case ApiRequestTypeGetPlaylist:
		data, ok := req.Data.(ApiRequestDataGetMetadata)
		if !ok {
			return "", 0, false
		}

		imageSize := normalizeImageSizeCacheKey(data.ImageSize, p.app.cfg.Server.ImageSize)
		return fmt.Sprintf("%s:%s:%s:%d:%d", req.Type, data.Id, imageSize, data.Limit, data.Offset), apiResponseCacheTTLPlaylist, true
	default:
		return "", 0, false
	}
}

func (p *AppPlayer) getApiResponseCache(req ApiRequest) (any, bool) {
	key, _, ok := p.apiResponseCacheDescriptor(req)
	if !ok {
		return nil, false
	}

	p.apiResponseCacheLock.RLock()
	defer p.apiResponseCacheLock.RUnlock()

	entry, ok := p.apiResponseCache[key]
	if !ok {
		return nil, false
	}
	if entry.TTL <= 0 || time.Since(entry.CachedAt) > entry.TTL {
		return nil, false
	}

	return entry.Value, true
}

func (p *AppPlayer) putApiResponseCache(req ApiRequest, value any) {
	key, ttl, ok := p.apiResponseCacheDescriptor(req)
	if !ok || ttl <= 0 || value == nil {
		return
	}

	p.apiResponseCacheLock.Lock()
	defer p.apiResponseCacheLock.Unlock()

	if p.apiResponseCache == nil {
		p.apiResponseCache = make(map[string]apiResponseCacheEntry)
	}

	if _, exists := p.apiResponseCache[key]; !exists {
		p.apiResponseCacheOrder = append(p.apiResponseCacheOrder, key)
	}
	p.apiResponseCache[key] = apiResponseCacheEntry{Value: value, CachedAt: time.Now(), TTL: ttl}

	for len(p.apiResponseCacheOrder) > apiResponseCacheMaxEntries {
		evict := p.apiResponseCacheOrder[0]
		p.apiResponseCacheOrder = p.apiResponseCacheOrder[1:]
		delete(p.apiResponseCache, evict)
	}
}

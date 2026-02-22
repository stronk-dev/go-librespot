package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type imageProxyCacheEntry struct {
	Body        []byte
	ContentType string
	ETag        string
	CachedAt    time.Time
}

const (
	imageProxyCacheMaxEntries = 2048
	imageProxyCacheTTL        = 24 * time.Hour
	imageProxyFetchLimitBytes = 10 << 20
	imageProxyCacheControl    = "public, max-age=86400"
)

func localImagePathFromHexID(imageID string) string {
	imageID = strings.ToLower(strings.TrimSpace(imageID))
	if !isHexImageID(imageID) {
		return ""
	}
	return "/image/" + imageID
}

func localImagePathFromFileID(fileID []byte) string {
	if len(fileID) == 0 {
		return ""
	}
	return localImagePathFromHexID(hex.EncodeToString(fileID))
}

func localizeSpotifyImageURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	if strings.HasPrefix(raw, "spotify:image:") {
		imageID := strings.TrimPrefix(raw, "spotify:image:")
		if localPath := localImagePathFromHexID(imageID); localPath != "" {
			return localPath
		}
		return raw
	}

	if localPath := localImagePathFromHexID(raw); localPath != "" {
		return localPath
	}

	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if strings.EqualFold(u.Host, "u.scdn.co") {
		path := strings.Trim(u.Path, "/")
		if path != "" {
			parts := strings.Split(path, "/")
			imageID := parts[len(parts)-1]
			if localPath := localImagePathFromHexID(imageID); localPath != "" {
				return localPath
			}
		}
	}

	if strings.EqualFold(u.Host, "i.scdn.co") {
		path := strings.Trim(u.Path, "/")
		if strings.HasPrefix(path, "image/") {
			imageID := strings.TrimPrefix(path, "image/")
			if localPath := localImagePathFromHexID(imageID); localPath != "" {
				return localPath
			}
		}
	}

	return raw
}

func (p *AppPlayer) getImageProxyCache(imageID string) (imageProxyCacheEntry, bool) {
	p.imageProxyCacheLock.RLock()
	defer p.imageProxyCacheLock.RUnlock()

	entry, ok := p.imageProxyCache[imageID]
	if !ok {
		return imageProxyCacheEntry{}, false
	}
	if time.Since(entry.CachedAt) > imageProxyCacheTTL {
		return imageProxyCacheEntry{}, false
	}

	return imageProxyCacheEntry{
		Body:        cloneBytes(entry.Body),
		ContentType: entry.ContentType,
		ETag:        entry.ETag,
		CachedAt:    entry.CachedAt,
	}, true
}

func (p *AppPlayer) putImageProxyCache(imageID string, entry imageProxyCacheEntry) {
	p.imageProxyCacheLock.Lock()
	defer p.imageProxyCacheLock.Unlock()

	if p.imageProxyCache == nil {
		p.imageProxyCache = make(map[string]imageProxyCacheEntry)
	}

	if _, exists := p.imageProxyCache[imageID]; !exists {
		p.imageProxyCacheOrder = append(p.imageProxyCacheOrder, imageID)
	}
	p.imageProxyCache[imageID] = imageProxyCacheEntry{
		Body:        cloneBytes(entry.Body),
		ContentType: entry.ContentType,
		ETag:        entry.ETag,
		CachedAt:    time.Now(),
	}

	for len(p.imageProxyCacheOrder) > imageProxyCacheMaxEntries {
		evict := p.imageProxyCacheOrder[0]
		p.imageProxyCacheOrder = p.imageProxyCacheOrder[1:]
		delete(p.imageProxyCache, evict)
	}
}

func (p *AppPlayer) imageProxySourceURL(imageID string) string {
	if p != nil && p.prodInfo != nil {
		if fileID, err := hex.DecodeString(imageID); err == nil {
			if sourceURL := p.prodInfo.ImageUrl(fileID); sourceURL != nil && *sourceURL != "" {
				return *sourceURL
			}
		}
	}

	return "https://i.scdn.co/image/" + imageID
}

func (p *AppPlayer) fetchImageProxyContent(ctx context.Context, imageID string) (imageProxyCacheEntry, error) {
	sourceURL := p.imageProxySourceURL(imageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return imageProxyCacheEntry{}, fmt.Errorf("invalid image proxy request: %w", err)
	}

	resp, err := p.app.client.Do(req)
	if err != nil {
		return imageProxyCacheEntry{}, fmt.Errorf("image proxy request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// Continue.
	case http.StatusNotFound:
		return imageProxyCacheEntry{}, ErrNotFound
	case http.StatusForbidden:
		return imageProxyCacheEntry{}, ErrForbidden
	case http.StatusTooManyRequests:
		return imageProxyCacheEntry{}, ErrTooManyRequests
	default:
		return imageProxyCacheEntry{}, fmt.Errorf("image proxy request failed with status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, imageProxyFetchLimitBytes))
	if err != nil {
		return imageProxyCacheEntry{}, fmt.Errorf("failed reading image proxy response body: %w", err)
	}
	if len(body) == 0 {
		return imageProxyCacheEntry{}, fmt.Errorf("empty image proxy response")
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}

	return imageProxyCacheEntry{
		Body:        body,
		ContentType: contentType,
		ETag:        makeBinaryETag(body),
		CachedAt:    time.Now(),
	}, nil
}

func (p *AppPlayer) resolveImageProxyBinary(ctx context.Context, data ApiRequestDataGetImage) (*ApiResponseBinary, error) {
	imageID := strings.ToLower(strings.TrimSpace(data.Id))
	if !isHexImageID(imageID) {
		return nil, ErrBadRequest
	}

	if cached, ok := p.getImageProxyCache(imageID); ok {
		if data.IfNoneMatch != "" && cached.ETag != "" && data.IfNoneMatch == cached.ETag {
			return &ApiResponseBinary{
				StatusCode:   http.StatusNotModified,
				ContentType:  cached.ContentType,
				CacheControl: imageProxyCacheControl,
				ETag:         cached.ETag,
			}, nil
		}

		return &ApiResponseBinary{
			ContentType:  cached.ContentType,
			CacheControl: imageProxyCacheControl,
			ETag:         cached.ETag,
			Body:         cloneBytes(cached.Body),
		}, nil
	}

	entry, err := p.fetchImageProxyContent(ctx, imageID)
	if err != nil {
		return nil, err
	}
	p.putImageProxyCache(imageID, entry)

	if data.IfNoneMatch != "" && entry.ETag != "" && data.IfNoneMatch == entry.ETag {
		return &ApiResponseBinary{
			StatusCode:   http.StatusNotModified,
			ContentType:  entry.ContentType,
			CacheControl: imageProxyCacheControl,
			ETag:         entry.ETag,
		}, nil
	}

	return &ApiResponseBinary{
		ContentType:  entry.ContentType,
		CacheControl: imageProxyCacheControl,
		ETag:         entry.ETag,
		Body:         cloneBytes(entry.Body),
	}, nil
}

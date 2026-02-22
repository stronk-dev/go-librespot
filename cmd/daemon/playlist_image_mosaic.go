package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"image"
	imagedraw "image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

type playlistImageCacheEntry struct {
	JPEG []byte
	ETag string
}

const (
	playlistMosaicCacheMaxEntries = 128
	playlistMosaicDefaultSizePx   = 300
	playlistMosaicTileCount       = 2
	playlistMosaicCoverCount      = playlistMosaicTileCount * playlistMosaicTileCount
	playlistMosaicTrackProbeLimit = 64
	playlistMosaicFetchLimitBytes = 5 << 20
	playlistMosaicCacheControl    = "public, max-age=300"
)

func playlistImageCacheKey(playlistID string, revision []byte, imageSize string) string {
	return fmt.Sprintf("%s:%s:%s", playlistID, hex.EncodeToString(revision), strings.ToLower(strings.TrimSpace(imageSize)))
}

func playlistMosaicRenderSize(size int) int {
	switch size {
	case 60, 300, 640:
		return size
	default:
		return playlistMosaicDefaultSizePx
	}
}

func playlistMosaicTrackImageSize(size int) string {
	switch size {
	case 60:
		return "small"
	case 640:
		return "xlarge"
	default:
		return "default"
	}
}

func playlistMosaicOutputSizeFromImageSize(size string) int {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "small":
		return 60
	case "large", "xlarge":
		return 640
	default:
		return playlistMosaicDefaultSizePx
	}
}

func playlistGeneratedImagePath(playlistID string, size int) string {
	return fmt.Sprintf("/metadata/playlist/%s/image?size=%d", url.PathEscape(playlistID), playlistMosaicRenderSize(size))
}

func cloneBytes(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out
}

func makeBinaryETag(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	sum := sha1.Sum(payload)
	return fmt.Sprintf("\"%x\"", sum)
}

func (p *AppPlayer) getPlaylistImageCache(key string) (playlistImageCacheEntry, bool) {
	p.playlistImageCacheLock.RLock()
	defer p.playlistImageCacheLock.RUnlock()

	entry, ok := p.playlistImageCache[key]
	if !ok {
		return playlistImageCacheEntry{}, false
	}

	return playlistImageCacheEntry{
		JPEG: cloneBytes(entry.JPEG),
		ETag: entry.ETag,
	}, true
}

func (p *AppPlayer) putPlaylistImageCache(key string, entry playlistImageCacheEntry) {
	p.playlistImageCacheLock.Lock()
	defer p.playlistImageCacheLock.Unlock()

	if p.playlistImageCache == nil {
		p.playlistImageCache = make(map[string]playlistImageCacheEntry)
	}
	if _, exists := p.playlistImageCache[key]; !exists {
		p.playlistImageCacheOrder = append(p.playlistImageCacheOrder, key)
	}
	p.playlistImageCache[key] = playlistImageCacheEntry{
		JPEG: cloneBytes(entry.JPEG),
		ETag: entry.ETag,
	}

	for len(p.playlistImageCacheOrder) > playlistMosaicCacheMaxEntries {
		evict := p.playlistImageCacheOrder[0]
		p.playlistImageCacheOrder = p.playlistImageCacheOrder[1:]
		delete(p.playlistImageCache, evict)
	}
}

func resizeImageNearest(src image.Image, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	if src == nil || width <= 0 || height <= 0 {
		return dst
	}

	srcBounds := src.Bounds()
	srcW, srcH := srcBounds.Dx(), srcBounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return dst
	}

	for y := 0; y < height; y++ {
		srcY := srcBounds.Min.Y + y*srcH/height
		for x := 0; x < width; x++ {
			srcX := srcBounds.Min.X + x*srcW/width
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}

	return dst
}

func (p *AppPlayer) fetchImage(ctx context.Context, rawURL string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid image request: %w", err)
	}

	resp, err := p.app.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed downloading image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid status code downloading image: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, playlistMosaicFetchLimitBytes))
	if err != nil {
		return nil, fmt.Errorf("failed reading image response: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty image response")
	}

	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed decoding image: %w", err)
	}

	return img, nil
}

func (p *AppPlayer) buildPlaylistMosaicJPEG(ctx context.Context, coverURLs []string, size int) ([]byte, error) {
	images := make([]image.Image, 0, playlistMosaicCoverCount)
	for _, coverURL := range coverURLs {
		if coverURL == "" {
			continue
		}
		img, err := p.fetchImage(ctx, coverURL)
		if err != nil {
			continue
		}
		images = append(images, img)
		if len(images) >= playlistMosaicCoverCount {
			break
		}
	}

	if len(images) == 0 {
		return nil, fmt.Errorf("no playlist cover images available")
	}
	for len(images) < playlistMosaicCoverCount {
		images = append(images, images[len(images)-1])
	}

	mosaicSize := playlistMosaicRenderSize(size)
	tileSize := mosaicSize / playlistMosaicTileCount
	canvas := image.NewRGBA(image.Rect(0, 0, mosaicSize, mosaicSize))
	for i, src := range images[:playlistMosaicCoverCount] {
		col := i % playlistMosaicTileCount
		row := i / playlistMosaicTileCount
		dstRect := image.Rect(col*tileSize, row*tileSize, (col+1)*tileSize, (row+1)*tileSize)
		scaled := resizeImageNearest(src, tileSize, tileSize)
		imagedraw.Draw(canvas, dstRect, scaled, image.Point{}, imagedraw.Src)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("failed encoding playlist mosaic: %w", err)
	}

	return buf.Bytes(), nil
}

func (p *AppPlayer) playlistTrackCoverURL(ctx context.Context, trackURI, imageSize string) (string, error) {
	trackID, err := librespot.SpotifyIdFromUri(trackURI)
	if err != nil {
		return "", err
	}
	if trackID.Type() != librespot.SpotifyIdTypeTrack {
		return "", fmt.Errorf("not a track uri")
	}

	var trackMeta metadatapb.Track
	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *trackID, extmetadatapb.ExtensionKind_TRACK_V4, &trackMeta); err != nil {
		return "", err
	}

	if trackMeta.Album == nil {
		return "", fmt.Errorf("track has no album metadata")
	}

	fileID := getBestImageIdForSize(trackMeta.Album.Cover, imageSize)
	if fileID == nil && trackMeta.Album.CoverGroup != nil {
		fileID = getBestImageIdForSize(trackMeta.Album.CoverGroup.Image, imageSize)
	}
	if len(fileID) == 0 {
		return "", fmt.Errorf("track has no cover image")
	}

	return p.imageProxySourceURL(hex.EncodeToString(fileID)), nil
}

func (p *AppPlayer) resolvePlaylistGeneratedImage(ctx context.Context, resolver contextTrackPager, playlistID string, revision []byte, size int) ([]byte, string, error) {
	trackImageSize := playlistMosaicTrackImageSize(size)
	cacheKey := playlistImageCacheKey(playlistID, revision, fmt.Sprintf("%d:%s", size, trackImageSize))
	if cached, ok := p.getPlaylistImageCache(cacheKey); ok && len(cached.JPEG) > 0 {
		return cached.JPEG, cached.ETag, nil
	}

	seedItems, err := playlistItemsFromPager(ctx, resolver, 0, playlistMosaicTrackProbeLimit)
	if err != nil {
		return nil, "", err
	}

	coverURLs := make([]string, 0, playlistMosaicCoverCount)
	seenCoverURLs := make(map[string]struct{})
	for _, item := range seedItems {
		coverURL, err := p.playlistTrackCoverURL(ctx, item.Uri, trackImageSize)
		if err != nil || coverURL == "" {
			continue
		}
		if _, ok := seenCoverURLs[coverURL]; ok {
			continue
		}
		seenCoverURLs[coverURL] = struct{}{}
		coverURLs = append(coverURLs, coverURL)
		if len(coverURLs) >= playlistMosaicCoverCount {
			break
		}
	}

	if len(coverURLs) == 0 {
		return nil, "", fmt.Errorf("unable to resolve track cover urls")
	}

	mosaicJPEG, err := p.buildPlaylistMosaicJPEG(ctx, coverURLs, size)
	if err != nil {
		return nil, "", err
	}
	etag := makeBinaryETag(mosaicJPEG)
	entry := playlistImageCacheEntry{JPEG: mosaicJPEG, ETag: etag}
	p.putPlaylistImageCache(cacheKey, entry)

	return cloneBytes(mosaicJPEG), etag, nil
}

func (p *AppPlayer) resolvePlaylistGeneratedImagePath(ctx context.Context, resolver contextTrackPager, playlistID string, revision []byte, size int) (string, error) {
	if _, _, err := p.resolvePlaylistGeneratedImage(ctx, resolver, playlistID, revision, size); err != nil {
		return "", err
	}
	return playlistGeneratedImagePath(playlistID, size), nil
}

func (p *AppPlayer) setGeneratedPlaylistImageURL(ctx context.Context, resp *ApiResponsePlaylistFull, resolver contextTrackPager, playlistID string, revision []byte, imageSize string) {
	if resp == nil || resp.ImageUrl != nil {
		return
	}

	mosaicPath, err := p.resolvePlaylistGeneratedImagePath(ctx, resolver, playlistID, revision, playlistMosaicOutputSizeFromImageSize(imageSize))
	if err != nil {
		p.app.log.WithError(err).WithField("playlist_id", playlistID).Debug("failed generating playlist mosaic image")
		return
	}

	resp.ImageUrl = &mosaicPath
}

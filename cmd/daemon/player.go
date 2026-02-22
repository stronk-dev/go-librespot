package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devgianlu/go-librespot/mpris"
	"github.com/godbus/dbus/v5"
	"google.golang.org/protobuf/proto"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	"github.com/devgianlu/go-librespot/dealer"
	"github.com/devgianlu/go-librespot/player"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	playerpb "github.com/devgianlu/go-librespot/proto/spotify/player"
	playlist4pb "github.com/devgianlu/go-librespot/proto/spotify/playlist4"
	"github.com/devgianlu/go-librespot/session"
	spclientpkg "github.com/devgianlu/go-librespot/spclient"
	"github.com/devgianlu/go-librespot/tracks"
)

type AppPlayer struct {
	app  *App
	sess *session.Session

	stop   chan struct{}
	logout chan *AppPlayer

	player            *player.Player
	initialVolumeOnce sync.Once
	volumeUpdate      chan float32

	spotConnId string

	prodInfo    *ProductInfo
	countryCode *string

	state           *State
	primaryStream   *player.Stream
	secondaryStream *player.Stream

	prefetchTimer *time.Timer

	playlistImageCacheLock  sync.RWMutex
	playlistImageCache      map[string]playlistImageCacheEntry
	playlistImageCacheOrder []string

	imageProxyCacheLock  sync.RWMutex
	imageProxyCache      map[string]imageProxyCacheEntry
	imageProxyCacheOrder []string

	apiResponseCacheLock  sync.RWMutex
	apiResponseCache      map[string]apiResponseCacheEntry
	apiResponseCacheOrder []string

	rootlistCacheLock  sync.RWMutex
	rootlistCacheItems []ApiResponseRootlistItem
	rootlistCacheAt    time.Time

	ownerDisplayNameCacheLock  sync.RWMutex
	ownerDisplayNameCache      map[string]displayNameCacheEntry
	ownerDisplayNameCacheOrder []string
}

func (p *AppPlayer) handleAccesspointPacket(pktType ap.PacketType, payload []byte) error {
	switch pktType {
	case ap.PacketTypeProductInfo:
		var prod ProductInfo
		if err := xml.Unmarshal(payload, &prod); err != nil {
			return fmt.Errorf("failed umarshalling ProductInfo: %w", err)
		}

		if len(prod.Products) != 1 {
			return fmt.Errorf("invalid ProductInfo")
		}

		p.prodInfo = &prod
		return nil
	case ap.PacketTypeCountryCode:
		*p.countryCode = string(payload)
		return nil
	default:
		return nil
	}
}

func (p *AppPlayer) handleDealerMessage(ctx context.Context, msg dealer.Message) error {
	// Limit ourselves to 30 seconds for handling dealer messages
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if strings.HasPrefix(msg.Uri, "hm://pusher/v1/connections/") {
		p.spotConnId = msg.Headers["Spotify-Connection-Id"]
		p.app.log.Debugf("received connection id: %s...%s", p.spotConnId[:16], p.spotConnId[len(p.spotConnId)-16:])

		// put the initial state
		if err := p.putConnectState(ctx, connectpb.PutStateReason_NEW_DEVICE); err != nil {
			return fmt.Errorf("failed initial state put: %w", err)
		}

		if !p.app.cfg.ExternalVolume && len(p.app.cfg.MixerDevice) == 0 {
			// update initial volume
			p.initialVolumeOnce.Do(func() {
				if lastVolume := p.app.state.LastVolume; !p.app.cfg.IgnoreLastVolume && lastVolume != nil {
					p.updateVolume(*lastVolume)
				} else {
					p.updateVolume(p.app.cfg.InitialVolume * player.MaxStateVolume / p.app.cfg.VolumeSteps)
				}
			})
		}
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/connect/volume") {
		var setVolCmd connectpb.SetVolumeCommand
		if err := proto.Unmarshal(msg.Payload, &setVolCmd); err != nil {
			return fmt.Errorf("failed unmarshalling SetVolumeCommand: %w", err)
		}

		p.updateVolume(uint32(setVolCmd.Volume))
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/connect/logout") {
		// this should happen only with zeroconf enabled
		p.app.log.WithField("username", librespot.ObfuscateUsername(p.sess.Username())).
			Debugf("requested logout out")
		p.logout <- p
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/cluster") {
		var clusterUpdate connectpb.ClusterUpdate
		if err := proto.Unmarshal(msg.Payload, &clusterUpdate); err != nil {
			return fmt.Errorf("failed unmarshalling ClusterUpdate: %w", err)
		}

		stopBeingActive := p.state.active && clusterUpdate.Cluster.ActiveDeviceId != p.app.deviceId && clusterUpdate.Cluster.PlayerState.Timestamp > p.state.lastTransferTimestamp

		// We are still the active device, do not quit
		if !stopBeingActive {
			return nil
		}

		name := "<unknown>"
		if device := clusterUpdate.Cluster.Device[clusterUpdate.Cluster.ActiveDeviceId]; device != nil {
			name = device.Name
		}
		p.app.log.Infof("playback was transferred to %s", name)

		return p.stopPlayback(ctx)
	}

	return nil
}

func (p *AppPlayer) handlePlayerCommand(ctx context.Context, req dealer.RequestPayload) error {
	p.state.lastCommand = &req

	p.app.log.Debugf("handling %s player command from %s", req.Command.Endpoint, req.SentByDeviceId)

	switch req.Command.Endpoint {
	case "transfer":
		if len(req.Command.Data) == 0 {
			p.app.server.Emit(&ApiEvent{
				Type: ApiEventTypeActive,
			})

			return nil
		}

		var transferState connectpb.TransferState
		if err := proto.Unmarshal(req.Command.Data, &transferState); err != nil {
			return fmt.Errorf("failed unmarshalling TransferState: %w", err)
		}
		p.state.lastTransferTimestamp = transferState.Playback.Timestamp

		ctxTracks, err := tracks.NewTrackListFromContext(ctx, p.app.log, p.sess.Spclient(), transferState.CurrentSession.Context)
		if err != nil {
			return fmt.Errorf("failed creating track list: %w", err)
		}

		if sessId := transferState.CurrentSession.OriginalSessionId; sessId != nil {
			p.state.player.SessionId = *sessId
		} else {
			sessionId := make([]byte, 16)
			_, _ = rand.Read(sessionId)
			p.state.player.SessionId = base64.StdEncoding.EncodeToString(sessionId)
		}

		p.state.setActive(true)
		p.state.player.IsPlaying = false
		p.state.player.IsBuffering = false

		// options
		p.state.player.Options = transferState.Options
		pause := transferState.Playback.IsPaused && req.Command.Options.RestorePaused != "resume"
		// playback
		// Note: this sets playback speed to 0 or 1 because that's all we're
		// capable of, depending on whether the playback is paused or not.
		p.state.player.Timestamp = transferState.Playback.Timestamp
		p.state.player.PositionAsOfTimestamp = int64(transferState.Playback.PositionAsOfTimestamp)
		p.state.setPaused(pause)

		// current session
		p.state.player.PlayOrigin = transferState.CurrentSession.PlayOrigin
		p.state.player.PlayOrigin.DeviceIdentifier = req.SentByDeviceId
		p.state.player.ContextUri = transferState.CurrentSession.Context.Uri
		p.state.player.ContextUrl = transferState.CurrentSession.Context.Url
		p.state.player.ContextRestrictions = transferState.CurrentSession.Context.Restrictions
		p.state.player.Suppressions = transferState.CurrentSession.Suppressions

		p.state.player.ContextMetadata = map[string]string{}
		for k, v := range transferState.CurrentSession.Context.Metadata {
			p.state.player.ContextMetadata[k] = v
		}
		for k, v := range ctxTracks.Metadata() {
			p.state.player.ContextMetadata[k] = v
		}

		contextSpotType := librespot.InferSpotifyIdTypeFromContextUri(p.state.player.ContextUri)
		currentTrack := librespot.ContextTrackToProvidedTrack(contextSpotType, transferState.Playback.CurrentTrack)
		if err := ctxTracks.TrySeek(ctx, tracks.ProvidedTrackComparator(contextSpotType, currentTrack)); err != nil {
			return fmt.Errorf("failed seeking to track: %w", err)
		}

		// shuffle the context if needed
		if err := ctxTracks.ToggleShuffle(ctx, transferState.Options.ShufflingContext); err != nil {
			return fmt.Errorf("failed shuffling context")
		}

		// Set queueID to the highest queue ID found in the queue.
		// The UIDs are of the form q0, q1, q2, etc.
		// Spotify apps don't seem to do this (they start again at 0 after
		// transfer), which means that queue IDs get duplicated when tracks are
		// added before and after the transfer and reordering will lead to weird
		// effects. But we can do better :)
		p.state.queueID = 0
		for _, track := range transferState.Queue.Tracks {
			if track.Uid == "" || track.Uid[0] != 'q' {
				continue // not of the "q<number>" format
			}
			n, err := strconv.ParseUint(track.Uid[1:], 10, 64)
			if err != nil {
				continue // not of the "q<number>" format
			}
			p.state.queueID = max(p.state.queueID, n)
		}

		// add all tracks from queue
		for _, track := range transferState.Queue.Tracks {
			ctxTracks.AddToQueue(track)
		}
		ctxTracks.SetPlayingQueue(transferState.Queue.IsPlayingQueue)

		p.state.tracks = ctxTracks
		p.state.player.Track = ctxTracks.CurrentTrack()
		p.state.player.PrevTracks = ctxTracks.PrevTracks()
		p.state.player.NextTracks = ctxTracks.NextTracks(ctx, nil)
		p.state.player.Index = ctxTracks.Index()

		// load current track into stream
		if err := p.loadCurrentTrack(ctx, pause, true); err != nil {
			return fmt.Errorf("failed loading current track (transfer): %w", err)
		}

		p.app.server.Emit(&ApiEvent{
			Type: ApiEventTypeActive,
		})

		return nil
	case "play":
		p.state.setActive(true)

		p.state.player.PlayOrigin = req.Command.PlayOrigin
		p.state.player.PlayOrigin.DeviceIdentifier = req.SentByDeviceId
		p.state.player.Suppressions = req.Command.Options.Suppressions

		// apply overrides
		if req.Command.Options.PlayerOptionsOverride != nil {
			p.state.player.Options.ShufflingContext = req.Command.Options.PlayerOptionsOverride.ShufflingContext
			p.state.player.Options.RepeatingTrack = req.Command.Options.PlayerOptionsOverride.RepeatingTrack
			p.state.player.Options.RepeatingContext = req.Command.Options.PlayerOptionsOverride.RepeatingContext
		}

		var skipTo skipToFunc
		if len(req.Command.Options.SkipTo.TrackUri) > 0 || len(req.Command.Options.SkipTo.TrackUid) > 0 || req.Command.Options.SkipTo.TrackIndex > 0 {
			index := -1
			skipTo = func(track *connectpb.ContextTrack) bool {
				if len(req.Command.Options.SkipTo.TrackUid) > 0 && req.Command.Options.SkipTo.TrackUid == track.Uid {
					return true
				} else if len(req.Command.Options.SkipTo.TrackUri) > 0 && req.Command.Options.SkipTo.TrackUri == track.Uri {
					return true
					// the following length checks are needed, because the TrackIndex corresponds to an offset relative to the current playlist or album
					// If there are multiple albums in the current context (e.g. when starting from an artists page, the TrackIndex would indicate, that
					// you started the xth track vom the first album, even if you started the xth track from the second or third album etc.)
				} else if req.Command.Options.SkipTo.TrackIndex != 0 && len(req.Command.Options.SkipTo.TrackUri) == 0 && len(req.Command.Options.SkipTo.TrackUid) == 0 {
					index += 1
					return index == req.Command.Options.SkipTo.TrackIndex
				} else {
					return false
				}
			}
		}

		return p.loadContext(ctx, req.Command.Context, skipTo, req.Command.Options.InitiallyPaused, true)
	case "pause":
		return p.pause(ctx)
	case "resume":
		return p.play(ctx)
	case "seek_to":
		var position int64
		if req.Command.Relative == "current" {
			position = p.player.PositionMs() + req.Command.Position
		} else if req.Command.Relative == "beginning" {
			position = req.Command.Position
		} else if req.Command.Relative == "" {
			if pos, ok := req.Command.Value.(float64); ok {
				position = int64(pos)
			} else {
				p.app.log.Warnf("unsupported seek_to position type: %T", req.Command.Value)
				return nil
			}
		} else {
			p.app.log.Warnf("unsupported seek_to relative position: %s", req.Command.Relative)
			return nil
		}

		if err := p.seek(ctx, position); err != nil {
			return fmt.Errorf("failed seeking stream: %w", err)
		}

		return nil
	case "skip_prev":
		return p.skipPrev(ctx, req.Command.Options.AllowSeeking)
	case "skip_next":
		return p.skipNext(ctx, req.Command.Track)
	case "update_context":
		if req.Command.Context.Uri != p.state.player.ContextUri {
			p.app.log.Warnf("ignoring context update for wrong uri: %s", req.Command.Context.Uri)
			return nil
		}

		p.state.player.ContextRestrictions = req.Command.Context.Restrictions
		if p.state.player.ContextMetadata == nil {
			p.state.player.ContextMetadata = map[string]string{}
		}
		for k, v := range req.Command.Context.Metadata {
			p.state.player.ContextMetadata[k] = v
		}

		p.updateState(ctx)
		return nil
	case "set_repeating_context":
		val := req.Command.Value.(bool)
		p.setOptions(ctx, &val, nil, nil)
		return nil
	case "set_repeating_track":
		val := req.Command.Value.(bool)
		p.setOptions(ctx, nil, &val, nil)
		return nil
	case "set_shuffling_context":
		val := req.Command.Value.(bool)
		p.setOptions(ctx, nil, nil, &val)
		return nil
	case "set_options":
		p.setOptions(ctx, req.Command.RepeatingContext, req.Command.RepeatingTrack, req.Command.ShufflingContext)
		return nil
	case "set_queue":
		p.setQueue(ctx, req.Command.PrevTracks, req.Command.NextTracks)
		return nil
	case "add_to_queue":
		p.addToQueue(ctx, req.Command.Track)
		return nil
	default:
		return fmt.Errorf("unsupported player command: %s", req.Command.Endpoint)
	}
}

func (p *AppPlayer) handleDealerRequest(ctx context.Context, req dealer.Request) error {
	// Limit ourselves to 30 seconds for handling dealer requests
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch req.MessageIdent {
	case "hm://connect-state/v1/player/command":
		return p.handlePlayerCommand(ctx, req.Payload)
	default:
		p.app.log.Warnf("unknown dealer request: %s", req.MessageIdent)
		return nil
	}
}

func metadataImageSize(data ApiRequestDataGetMetadata, defaultSize string) string {
	if data.ImageSize != "" {
		return data.ImageSize
	}
	return defaultSize
}

func spotifyIDFromBase62(typ librespot.SpotifyIdType, id string) (librespot.SpotifyId, error) {
	spotId, err := librespot.SpotifyIdFromBase62(typ, id)
	if err != nil {
		return librespot.SpotifyId{}, ErrBadRequest
	}
	return *spotId, nil
}

func (p *AppPlayer) fetchExtendedMetadata(ctx context.Context, typ librespot.SpotifyIdType, data ApiRequestDataGetMetadata, ext extmetadatapb.ExtensionKind, out proto.Message, entityName string) (librespot.SpotifyId, error) {
	spotId, err := spotifyIDFromBase62(typ, data.Id)
	if err != nil {
		return librespot.SpotifyId{}, err
	}

	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, spotId, ext, out); err != nil {
		return librespot.SpotifyId{}, fmt.Errorf("failed getting %s metadata: %w", entityName, err)
	}

	return spotId, nil
}

func queueTrackFromProvidedTrack(track *connectpb.ProvidedTrack) ApiResponseQueueTrack {
	if track == nil {
		return ApiResponseQueueTrack{}
	}

	out := ApiResponseQueueTrack{
		Uri:      track.Uri,
		Provider: track.Provider,
	}
	if name, ok := track.Metadata["title"]; ok {
		out.Name = name
	}
	return out
}

func queueTracksFromProvidedTracks(tracks []*connectpb.ProvidedTrack) []ApiResponseQueueTrack {
	if len(tracks) == 0 {
		return []ApiResponseQueueTrack{}
	}

	out := make([]ApiResponseQueueTrack, 0, len(tracks))
	for _, track := range tracks {
		if track == nil {
			continue
		}
		out = append(out, queueTrackFromProvidedTrack(track))
	}
	return out
}

func (p *AppPlayer) resolveUserContext(ctx context.Context, suffix string) (*connectpb.Context, error) {
	return p.sess.Spclient().ContextResolve(ctx, fmt.Sprintf("spotify:user:%s:%s", p.sess.Username(), suffix))
}

func mercuryStatusCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}

	var statusCode int
	if _, scanErr := fmt.Sscanf(err.Error(), "mercury request failed with status code: %d", &statusCode); scanErr != nil {
		return 0, false
	}

	return statusCode, true
}

func contextResolveStatusCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}

	var statusCode int
	if _, scanErr := fmt.Sscanf(err.Error(), "invalid status code from context resolve: %d", &statusCode); scanErr != nil {
		return 0, false
	}

	return statusCode, true
}

func rootlistMercuryCandidates(username string) []string {
	escapedUsername := url.PathEscape(username)
	return []string{
		fmt.Sprintf("hm://playlist/user/%s/rootlist", escapedUsername),
		fmt.Sprintf("hm://playlist/v2/user/%s/rootlist", escapedUsername),
	}
}

func parseRootlistFromSelectedList(content *playlist4pb.SelectedListContent) []ApiResponseRootlistItem {
	if content == nil || content.Contents == nil || len(content.Contents.Items) == 0 {
		return []ApiResponseRootlistItem{}
	}

	seen := make(map[string]struct{}, len(content.Contents.Items))
	items := make([]ApiResponseRootlistItem, 0, len(content.Contents.Items))
	for i, item := range content.Contents.Items {
		if item == nil || item.GetUri() == "" {
			continue
		}

		normalizedURI, ok := normalizeRootlistPlaylistURI(item.GetUri())
		if !ok {
			continue
		}
		if _, exists := seen[normalizedURI]; exists {
			continue
		}
		seen[normalizedURI] = struct{}{}

		out := ApiResponseRootlistItem{Uri: normalizedURI}
		if i < len(content.Contents.MetaItems) {
			meta := content.Contents.MetaItems[i]
			if meta != nil && meta.Attributes != nil {
				out.Name = meta.Attributes.GetName()
			}
		}

		items = append(items, out)
	}

	return items
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func parseInt64(s string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func sanitizeContextDescription(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Context metadata sometimes includes numeric counters in description-like fields.
	if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return ""
	}
	return raw
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptyFromMap(meta map[string]string, keys ...string) string {
	if len(meta) == 0 {
		return ""
	}
	for _, key := range keys {
		if val := strings.TrimSpace(meta[key]); val != "" {
			return val
		}
	}
	return ""
}

func isHexImageID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

func normalizePlaylistImageURL(raw string) string {
	return localizeSpotifyImageURL(raw)
}

func playlistOwnerFromContextURI(uri string) string {
	const (
		userPrefix = "spotify:user:"
		plMarker   = ":playlist:"
	)
	if !strings.HasPrefix(uri, userPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(uri, userPrefix)
	idx := strings.Index(rest, plMarker)
	if idx <= 0 {
		return ""
	}
	return rest[:idx]
}

type contextTrackPager interface {
	Page(ctx context.Context, idx int) ([]*connectpb.ContextTrack, error)
}

func playlistItemFromContextTrack(track *connectpb.ContextTrack) (ApiResponsePlaylistItem, bool) {
	if track == nil || track.Uri == "" {
		return ApiResponsePlaylistItem{}, false
	}

	pi := ApiResponsePlaylistItem{
		Uri:     track.Uri,
		AddedBy: track.Metadata["added_by"],
	}
	if addedAtRaw := firstNonEmpty(track.Metadata["added_at"], track.Metadata["timestamp"]); addedAtRaw != "" {
		if ts, ok := parseInt64(addedAtRaw); ok {
			pi.AddedAt = ts
		}
	}

	return pi, true
}

func playlistItemsFromPager(ctx context.Context, pager contextTrackPager, offset, limit int) ([]ApiResponsePlaylistItem, error) {
	if offset < 0 {
		offset = 0
	}

	items := make([]ApiResponsePlaylistItem, 0)
	skipped := 0
	for pageIdx := 0; ; pageIdx++ {
		tracks, err := pager.Page(ctx, pageIdx)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}

		for _, track := range tracks {
			pi, ok := playlistItemFromContextTrack(track)
			if !ok {
				continue
			}
			if skipped < offset {
				skipped++
				continue
			}

			items = append(items, pi)
			if limit > 0 && len(items) >= limit {
				return items, nil
			}
		}
	}

	return items, nil
}

func pagePlaylistItems(items []ApiResponsePlaylistItem, offset, limit int) []ApiResponsePlaylistItem {
	start := offset
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []ApiResponsePlaylistItem{}
	}

	end := len(items)
	if limit > 0 && start+limit < end {
		end = start + limit
	}

	return items[start:end]
}

func parsePlaylistFromContext(spotCtx *connectpb.Context, fallbackURI string, data ApiRequestDataGetMetadata) *ApiResponsePlaylistFull {
	if spotCtx == nil {
		return &ApiResponsePlaylistFull{Uri: fallbackURI, Items: []ApiResponsePlaylistItem{}}
	}

	ctxMeta := spotCtx.Metadata
	pageMeta := map[string]string{}
	for _, page := range spotCtx.Pages {
		if page != nil && len(page.Metadata) > 0 {
			pageMeta = page.Metadata
			break
		}
	}

	resp := &ApiResponsePlaylistFull{
		Uri:           firstNonEmpty(spotCtx.Uri, fallbackURI),
		Name:          firstNonEmpty(firstNonEmptyFromMap(ctxMeta, "name", "title", "context_name"), firstNonEmptyFromMap(pageMeta, "name", "title", "context_name")),
		Description:   sanitizeContextDescription(firstNonEmpty(firstNonEmptyFromMap(ctxMeta, "description", "subtitle", "context_description"), firstNonEmptyFromMap(pageMeta, "description", "subtitle", "context_description"))),
		OwnerUsername: firstNonEmpty(firstNonEmptyFromMap(ctxMeta, "owner_username", "owner", "playlist_owner"), playlistOwnerFromContextURI(spotCtx.Uri), playlistOwnerFromContextURI(fallbackURI)),
		OwnerDisplayName: firstNonEmpty(
			firstNonEmptyFromMap(ctxMeta, "owner_display_name", "owner_name", "playlist_owner_name"),
			firstNonEmptyFromMap(pageMeta, "owner_display_name", "owner_name", "playlist_owner_name"),
		),
		Items: []ApiResponsePlaylistItem{},
	}

	if val, ok := parseBool(firstNonEmpty(firstNonEmptyFromMap(ctxMeta, "collaborative", "is_collaborative"), firstNonEmptyFromMap(pageMeta, "collaborative", "is_collaborative"))); ok {
		resp.Collaborative = val
	}
	if imageURL := normalizePlaylistImageURL(firstNonEmpty(firstNonEmptyFromMap(ctxMeta, "image_url", "picture", "image_xlarge_url", "image_large_url"), firstNonEmptyFromMap(pageMeta, "image_url", "picture", "image_xlarge_url", "image_large_url"))); imageURL != "" {
		resp.ImageUrl = &imageURL
	}

	allItems := make([]ApiResponsePlaylistItem, 0)
	for _, page := range spotCtx.Pages {
		if page == nil {
			continue
		}
		for _, track := range page.Tracks {
			pi, ok := playlistItemFromContextTrack(track)
			if !ok {
				continue
			}
			allItems = append(allItems, pi)
		}
	}

	if totalRaw := firstNonEmpty(ctxMeta["playlist_number_of_tracks"], ctxMeta["total_tracks"]); totalRaw != "" {
		if total, ok := parseInt64(totalRaw); ok && total >= 0 {
			resp.TotalTracks = int(total)
		}
	}
	if resp.TotalTracks == 0 {
		resp.TotalTracks = len(allItems)
	}
	resp.Items = pagePlaylistItems(allItems, data.Offset, data.Limit)

	return resp
}

func normalizeRootlistPlaylistURI(uri string) (string, bool) {
	// Ignore folder/group delimiters and unknown entries; this endpoint is playlist-only.
	if strings.HasPrefix(uri, "spotify:start-group:") || strings.HasPrefix(uri, "spotify:end-group:") {
		return "", false
	}

	if strings.HasPrefix(uri, "spotify:playlist:") {
		id := strings.TrimPrefix(uri, "spotify:playlist:")
		if !isSpotifyBase62ID(id) {
			return "", false
		}
		return "spotify:playlist:" + id, true
	}

	const marker = ":playlist:"
	idx := strings.LastIndex(uri, marker)
	if strings.HasPrefix(uri, "spotify:user:") && idx > 0 {
		id := uri[idx+len(marker):]
		if !isSpotifyBase62ID(id) {
			return "", false
		}
		return "spotify:playlist:" + id, true
	}

	return "", false
}

func playlistMercuryCandidates(playlistID, ownerUsername string) []string {
	playlistID = url.PathEscape(strings.TrimSpace(playlistID))
	if playlistID == "" {
		return []string{}
	}

	candidates := []string{
		fmt.Sprintf("hm://playlist/v2/playlist/%s", playlistID),
		fmt.Sprintf("hm://playlist/playlist/%s", playlistID),
	}

	if ownerUsername != "" {
		ownerUsername = url.PathEscape(strings.TrimSpace(ownerUsername))
		if ownerUsername != "" {
			candidates = append(candidates,
				fmt.Sprintf("hm://playlist/user/%s/playlist/%s", ownerUsername, playlistID),
				fmt.Sprintf("hm://playlist/v2/user/%s/playlist/%s", ownerUsername, playlistID),
			)
		}
	}

	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, uri := range candidates {
		if _, ok := seen[uri]; ok {
			continue
		}
		seen[uri] = struct{}{}
		out = append(out, uri)
	}

	return out
}

func (p *AppPlayer) resolvePlaylistMercury(ctx context.Context, playlistID, ownerUsername string) (*playlist4pb.SelectedListContent, error) {
	var lastErr error
	for _, uri := range playlistMercuryCandidates(playlistID, ownerUsername) {
		payload, err := p.sess.Mercury().Request(ctx, "GET", uri, nil, nil)
		if err != nil {
			lastErr = err
			p.app.log.WithError(err).WithField("uri", uri).Debug("failed resolving playlist via mercury")
			if statusCode, ok := mercuryStatusCode(err); ok && statusCode == 404 {
				continue
			}
			continue
		}

		var content playlist4pb.SelectedListContent
		if err := proto.Unmarshal(payload, &content); err != nil {
			lastErr = fmt.Errorf("failed decoding playlist mercury payload: %w", err)
			p.app.log.WithError(lastErr).WithField("uri", uri).Debug("failed decoding playlist mercury payload")
			continue
		}

		return &content, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrNotFound
}

func (p *AppPlayer) applyPlaylistSelectedListMetadata(resp *ApiResponsePlaylistFull, content *playlist4pb.SelectedListContent) {
	if resp == nil || content == nil {
		return
	}

	if attrs := content.GetAttributes(); attrs != nil {
		if name := strings.TrimSpace(attrs.GetName()); name != "" {
			resp.Name = name
		}
		if description := strings.TrimSpace(attrs.GetDescription()); description != "" {
			resp.Description = description
		}
		if attrs.Collaborative != nil {
			resp.Collaborative = attrs.GetCollaborative()
		}

		for _, size := range attrs.GetPictureSize() {
			imageURL := normalizePlaylistImageURL(size.GetUrl())
			if imageURL == "" {
				continue
			}
			resp.ImageUrl = &imageURL
			break
		}

		if resp.ImageUrl == nil {
			if p.prodInfo != nil {
				if localPath := localImagePathFromFileID(attrs.GetPicture()); localPath != "" {
					resp.ImageUrl = &localPath
				} else {
					resp.ImageUrl = p.prodInfo.ImageUrl(attrs.GetPicture())
				}
			}
		}
	}

	if items := content.GetContents(); items != nil {
		for _, meta := range items.GetMetaItems() {
			if meta == nil {
				continue
			}

			if attrs := meta.GetAttributes(); attrs != nil {
				if resp.Name == "" {
					if name := strings.TrimSpace(attrs.GetName()); name != "" {
						resp.Name = name
					}
				}
				if resp.Description == "" {
					if description := strings.TrimSpace(attrs.GetDescription()); description != "" {
						resp.Description = description
					}
				}
				if resp.ImageUrl == nil {
					for _, size := range attrs.GetPictureSize() {
						imageURL := normalizePlaylistImageURL(size.GetUrl())
						if imageURL == "" {
							continue
						}
						resp.ImageUrl = &imageURL
						break
					}
				}
				if resp.ImageUrl == nil && p.prodInfo != nil {
					if localPath := localImagePathFromFileID(attrs.GetPicture()); localPath != "" {
						resp.ImageUrl = &localPath
					} else {
						resp.ImageUrl = p.prodInfo.ImageUrl(attrs.GetPicture())
					}
				}
			}

			if resp.OwnerUsername == "" {
				if owner := strings.TrimSpace(meta.GetOwnerUsername()); owner != "" {
					resp.OwnerUsername = owner
				}
			}
			if resp.TotalTracks == 0 && meta.Length != nil {
				resp.TotalTracks = int(meta.GetLength())
			}
			if resp.Name != "" && resp.Description != "" && resp.ImageUrl != nil && resp.OwnerUsername != "" && resp.TotalTracks > 0 {
				break
			}
		}
	}

	if owner := strings.TrimSpace(content.GetOwnerUsername()); owner != "" {
		resp.OwnerUsername = owner
	}
	if content.Length != nil {
		resp.TotalTracks = int(content.GetLength())
	}
}

func metadataKeys(meta map[string]string) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func pageRootlistItems(items []ApiResponseRootlistItem, offset, limit int) []ApiResponseRootlistItem {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return []ApiResponseRootlistItem{}
	}

	end := len(items)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}

	return items[offset:end]
}

func cloneRootlistItems(items []ApiResponseRootlistItem) []ApiResponseRootlistItem {
	if len(items) == 0 {
		return []ApiResponseRootlistItem{}
	}

	out := make([]ApiResponseRootlistItem, len(items))
	copy(out, items)
	return out
}

func (p *AppPlayer) getRootlistCache() ([]ApiResponseRootlistItem, bool) {
	p.rootlistCacheLock.RLock()
	defer p.rootlistCacheLock.RUnlock()

	if p.rootlistCacheAt.IsZero() {
		return nil, false
	}
	if time.Since(p.rootlistCacheAt) > rootlistCacheTTL {
		return nil, false
	}

	return cloneRootlistItems(p.rootlistCacheItems), true
}

func (p *AppPlayer) putRootlistCache(items []ApiResponseRootlistItem) {
	p.rootlistCacheLock.Lock()
	defer p.rootlistCacheLock.Unlock()

	p.rootlistCacheItems = cloneRootlistItems(items)
	p.rootlistCacheAt = time.Now()
}

const rootlistCacheTTL = 10 * time.Second

func (p *AppPlayer) resolveRootlistMercury(ctx context.Context) (_ []ApiResponseRootlistItem, lastErr error) {
	if cached, ok := p.getRootlistCache(); ok {
		return cached, nil
	}

	allNotFound := true
	for _, uri := range rootlistMercuryCandidates(p.sess.Username()) {
		payload, err := p.sess.Mercury().Request(ctx, "GET", uri, nil, nil)
		if err != nil {
			lastErr = err
			p.app.log.WithError(err).WithField("uri", uri).Debug("failed resolving rootlist via mercury")
			if statusCode, ok := mercuryStatusCode(err); !ok {
				allNotFound = false
			} else {
				switch statusCode {
				case 404:
					// Keep trying alternate internal URIs.
				case 403:
					return nil, ErrForbidden
				case 429:
					return nil, ErrTooManyRequests
				default:
					allNotFound = false
				}
			}
			continue
		}

		var content playlist4pb.SelectedListContent
		if err := proto.Unmarshal(payload, &content); err != nil {
			lastErr = fmt.Errorf("failed decoding rootlist mercury payload: %w", err)
			p.app.log.WithError(lastErr).WithField("uri", uri).Debug("failed decoding rootlist mercury payload")
			allNotFound = false
			continue
		}

		items := parseRootlistFromSelectedList(&content)
		p.putRootlistCache(items)
		return items, nil
	}

	if allNotFound {
		return nil, ErrNotFound
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("failed resolving rootlist via mercury")
	}

	return nil, lastErr
}

func (p *AppPlayer) handleApiRequest(ctx context.Context, req ApiRequest) (any, error) {
	// Limit ourselves to 30 seconds for handling API requests
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch req.Type {
	case ApiRequestTypeWebApi:
		data := req.Data.(ApiRequestDataWebApi)
		resp, err := p.sess.WebApi(ctx, data.Method, data.Path, data.Query, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to send web api request: %w", err)
		}

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read web api response body: %w", err)
		}

		return &ApiResponseWebApi{
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       body,
		}, nil
	case ApiRequestTypeStatus:
		resp := &ApiResponseStatus{
			Username:       p.sess.Username(),
			DeviceId:       p.app.deviceId,
			DeviceType:     p.app.deviceType.String(),
			DeviceName:     p.app.cfg.DeviceName,
			VolumeSteps:    p.app.cfg.VolumeSteps,
			Volume:         p.apiVolume(),
			RepeatContext:  p.state.player.Options.RepeatingContext,
			RepeatTrack:    p.state.player.Options.RepeatingTrack,
			ShuffleContext: p.state.player.Options.ShufflingContext,
			Stopped:        !p.state.player.IsPlaying,
			Paused:         p.state.player.IsPaused,
			Buffering:      p.state.player.IsBuffering,
			PlayOrigin:     p.state.player.PlayOrigin.FeatureIdentifier,
		}

		if p.primaryStream != nil && p.prodInfo != nil {
			resp.Track = p.newApiResponseStatusTrack(p.primaryStream.Media, p.state.trackPosition())
		}

		return resp, nil
	case ApiRequestTypeResume:
		_ = p.play(ctx)
		return nil, nil
	case ApiRequestTypePause:
		_ = p.pause(ctx)
		return nil, nil
	case ApiRequestTypePlayPause:
		if p.state.player.IsPaused {
			_ = p.play(ctx)
		} else {
			_ = p.pause(ctx)
		}
		return nil, nil
	case ApiRequestTypeSeek:
		data := req.Data.(ApiRequestDataSeek)

		var position int64
		if data.Relative {
			position = p.player.PositionMs() + data.Position
		} else {
			position = data.Position
		}

		_ = p.seek(ctx, position)
		return nil, nil
	case ApiRequestTypePrev:
		_ = p.skipPrev(ctx, true)
		return nil, nil
	case ApiRequestTypeNext:
		data := req.Data.(ApiRequestDataNext)
		if data.Uri != nil {
			_ = p.skipNext(ctx, &connectpb.ContextTrack{Uri: *data.Uri})
		} else {
			_ = p.skipNext(ctx, nil)
		}
		return nil, nil
	case ApiRequestTypePlay:
		data := req.Data.(ApiRequestDataPlay)
		spotCtx, err := p.sess.Spclient().ContextResolve(ctx, data.Uri)
		if err != nil {
			return nil, fmt.Errorf("failed resolving context: %w", err)
		}

		p.state.setActive(true)
		p.state.setPaused(data.Paused)
		p.state.player.Suppressions = &connectpb.Suppressions{}
		p.state.player.PlayOrigin = &connectpb.PlayOrigin{
			FeatureIdentifier: "go-librespot",
			FeatureVersion:    librespot.VersionNumberString(),
		}

		var skipTo skipToFunc
		if len(data.SkipToUri) > 0 {
			skipToId, err := librespot.SpotifyIdFromUri(data.SkipToUri)
			if err != nil {
				p.app.log.WithError(err).Warnf("trying to skip to invalid uri: %s", data.SkipToUri)
				skipToId = nil
			}

			skipTo = func(track *connectpb.ContextTrack) bool {
				if len(track.Uri) > 0 {
					return data.SkipToUri == track.Uri
				} else if len(track.Gid) > 0 {
					return bytes.Equal(skipToId.Id(), track.Gid)
				} else {
					return false
				}
			}
		}

		if err := p.loadContext(ctx, spotCtx, skipTo, data.Paused, true); err != nil {
			return nil, fmt.Errorf("failed loading context: %w", err)
		}

		return nil, nil
	case ApiRequestTypeGetVolume:
		return &ApiResponseVolume{
			Max:   p.app.cfg.VolumeSteps,
			Value: p.apiVolume(),
		}, nil
	case ApiRequestTypeSetVolume:
		data := req.Data.(ApiRequestDataVolume)

		var volume int32
		if data.Relative {
			volume = int32(p.apiVolume())
			volume += data.Volume
			volume = max(min(volume, int32(p.app.cfg.VolumeSteps)), 0)
		} else {
			volume = data.Volume
		}

		p.updateVolume(uint32(volume) * player.MaxStateVolume / p.app.cfg.VolumeSteps)
		return nil, nil
	case ApiRequestTypeSetRepeatingContext:
		val := req.Data.(bool)
		p.setOptions(ctx, &val, nil, nil)
		return nil, nil
	case ApiRequestTypeSetRepeatingTrack:
		val := req.Data.(bool)
		p.setOptions(ctx, nil, &val, nil)
		return nil, nil
	case ApiRequestTypeSetShufflingContext:
		val := req.Data.(bool)
		p.setOptions(ctx, nil, nil, &val)
		return nil, nil
	case ApiRequestTypeAddToQueue:
		p.addToQueue(ctx, &connectpb.ContextTrack{Uri: req.Data.(string)})
		return nil, nil
	case ApiRequestTypeToken:
		accessToken, err := p.sess.Spclient().GetAccessToken(ctx, true)
		if err != nil {
			return nil, fmt.Errorf("failed getting access token: %w", err)
		}
		return &ApiResponseToken{
			Token: accessToken,
		}, nil
	case ApiRequestTypeGetTrack:
		if cached, ok := p.getApiResponseCache(req); ok {
			return cached, nil
		}

		data := req.Data.(ApiRequestDataGetMetadata)
		var trackMeta metadatapb.Track
		if _, err := p.fetchExtendedMetadata(ctx, librespot.SpotifyIdTypeTrack, data, extmetadatapb.ExtensionKind_TRACK_V4, &trackMeta, "track"); err != nil {
			return nil, err
		}
		imageSize := metadataImageSize(data, p.app.cfg.Server.ImageSize)
		resp := p.convertTrackProto(&trackMeta, imageSize)
		p.putApiResponseCache(req, resp)
		return resp, nil
	case ApiRequestTypeGetAlbum:
		if cached, ok := p.getApiResponseCache(req); ok {
			return cached, nil
		}

		data := req.Data.(ApiRequestDataGetMetadata)
		var albumMeta metadatapb.Album
		if _, err := p.fetchExtendedMetadata(ctx, librespot.SpotifyIdTypeAlbum, data, extmetadatapb.ExtensionKind_ALBUM_V4, &albumMeta, "album"); err != nil {
			return nil, err
		}
		imageSize := metadataImageSize(data, p.app.cfg.Server.ImageSize)
		resp := p.convertAlbumProto(&albumMeta, imageSize)
		p.putApiResponseCache(req, resp)
		return resp, nil
	case ApiRequestTypeGetArtist:
		if cached, ok := p.getApiResponseCache(req); ok {
			return cached, nil
		}

		data := req.Data.(ApiRequestDataGetMetadata)
		var artistMeta metadatapb.Artist
		if _, err := p.fetchExtendedMetadata(ctx, librespot.SpotifyIdTypeArtist, data, extmetadatapb.ExtensionKind_ARTIST_V4, &artistMeta, "artist"); err != nil {
			return nil, err
		}
		imageSize := metadataImageSize(data, p.app.cfg.Server.ImageSize)
		countryCode := ""
		if p.countryCode != nil {
			countryCode = *p.countryCode
		}
		resp := p.convertArtistProto(&artistMeta, imageSize, countryCode)
		p.enrichArtistResponse(ctx, resp, imageSize)
		p.putApiResponseCache(req, resp)
		return resp, nil
	case ApiRequestTypeGetShow:
		if cached, ok := p.getApiResponseCache(req); ok {
			return cached, nil
		}

		data := req.Data.(ApiRequestDataGetMetadata)
		var showMeta metadatapb.Show
		if _, err := p.fetchExtendedMetadata(ctx, librespot.SpotifyIdTypeShow, data, extmetadatapb.ExtensionKind_SHOW_V4, &showMeta, "show"); err != nil {
			return nil, err
		}
		imageSize := metadataImageSize(data, p.app.cfg.Server.ImageSize)
		resp := p.convertShowProto(&showMeta, imageSize)
		p.putApiResponseCache(req, resp)
		return resp, nil
	case ApiRequestTypeGetEpisode:
		if cached, ok := p.getApiResponseCache(req); ok {
			return cached, nil
		}

		data := req.Data.(ApiRequestDataGetMetadata)
		var episodeMeta metadatapb.Episode
		if _, err := p.fetchExtendedMetadata(ctx, librespot.SpotifyIdTypeEpisode, data, extmetadatapb.ExtensionKind_EPISODE_V4, &episodeMeta, "episode"); err != nil {
			return nil, err
		}
		imageSize := metadataImageSize(data, p.app.cfg.Server.ImageSize)
		resp := p.convertEpisodeProto(&episodeMeta, imageSize)
		p.putApiResponseCache(req, resp)
		return resp, nil
	case ApiRequestTypeGetPlaylist:
		if cached, ok := p.getApiResponseCache(req); ok {
			return cached, nil
		}

		data := req.Data.(ApiRequestDataGetMetadata)
		spotId, err := spotifyIDFromBase62(librespot.SpotifyIdTypePlaylist, data.Id)
		if err != nil {
			return nil, err
		}
		playlistURI := spotId.Uri()
		spotCtx, err := p.sess.Spclient().ContextResolve(ctx, playlistURI)
		if err != nil {
			switch statusCode, ok := contextResolveStatusCode(err); {
			case ok && statusCode == 404:
				return nil, ErrNotFound
			case ok && statusCode == 403:
				return nil, ErrForbidden
			case ok && statusCode == 429:
				return nil, ErrTooManyRequests
			case ok && statusCode == 400:
				return nil, ErrBadRequest
			default:
				return nil, fmt.Errorf("failed resolving playlist context: %w", err)
			}
		}

		resp := parsePlaylistFromContext(spotCtx, playlistURI, ApiRequestDataGetMetadata{})
		var playlistMeta *playlist4pb.SelectedListContent
		if resolvedPlaylistMeta, err := p.resolvePlaylistMercury(ctx, spotId.Base62(), resp.OwnerUsername); err != nil {
			p.app.log.WithError(err).WithField("playlist_id", spotId.Base62()).Debug("failed enriching playlist metadata via mercury")
		} else {
			playlistMeta = resolvedPlaylistMeta
			p.applyPlaylistSelectedListMetadata(resp, playlistMeta)
		}
		resp.OwnerDisplayName = sanitizeResolvedDisplayName(resp.OwnerDisplayName, resp.OwnerUsername)
		if resp.OwnerDisplayName == "" && resp.OwnerUsername != "" {
			resp.OwnerDisplayName = p.resolveOwnerDisplayName(ctx, resp.OwnerUsername)
		}
		if resp.Name == "" {
			p.app.log.WithField("playlist_id", spotId.Base62()).
				WithField("context_meta_keys", strings.Join(metadataKeys(spotCtx.Metadata), ",")).
				Debug("playlist name unresolved after context+mercury enrichment")
		}
		resolver, err := spclientpkg.NewContextResolver(ctx, p.app.log, p.sess.Spclient(), spotCtx)
		if err != nil {
			return nil, fmt.Errorf("failed creating playlist context resolver: %w", err)
		}
		items, err := playlistItemsFromPager(ctx, resolver, data.Offset, data.Limit)
		if err != nil {
			return nil, fmt.Errorf("failed paging playlist context: %w", err)
		}
		var revision []byte
		if playlistMeta != nil {
			revision = playlistMeta.GetRevision()
		}

		if resp.TotalTracks == 0 {
			allItems, err := playlistItemsFromPager(ctx, resolver, 0, 0)
			if err != nil {
				return nil, fmt.Errorf("failed counting playlist context tracks: %w", err)
			}
			resp.TotalTracks = len(allItems)
			resp.Items = pagePlaylistItems(allItems, data.Offset, data.Limit)
			p.setGeneratedPlaylistImageURL(ctx, resp, resolver, spotId.Base62(), revision, data.ImageSize)
			p.putApiResponseCache(req, resp)
			return resp, nil
		}

		resp.Items = items
		p.setGeneratedPlaylistImageURL(ctx, resp, resolver, spotId.Base62(), revision, data.ImageSize)
		p.putApiResponseCache(req, resp)

		return resp, nil
	case ApiRequestTypeGetPlaylistImage:
		data := req.Data.(ApiRequestDataGetPlaylistImage)
		spotId, err := spotifyIDFromBase62(librespot.SpotifyIdTypePlaylist, data.Id)
		if err != nil {
			return nil, err
		}

		playlistURI := spotId.Uri()
		spotCtx, err := p.sess.Spclient().ContextResolve(ctx, playlistURI)
		if err != nil {
			switch statusCode, ok := contextResolveStatusCode(err); {
			case ok && statusCode == 404:
				return nil, ErrNotFound
			case ok && statusCode == 403:
				return nil, ErrForbidden
			case ok && statusCode == 429:
				return nil, ErrTooManyRequests
			case ok && statusCode == 400:
				return nil, ErrBadRequest
			default:
				return nil, fmt.Errorf("failed resolving playlist context: %w", err)
			}
		}

		baseResp := parsePlaylistFromContext(spotCtx, playlistURI, ApiRequestDataGetMetadata{})
		var revision []byte
		if playlistMeta, err := p.resolvePlaylistMercury(ctx, spotId.Base62(), baseResp.OwnerUsername); err == nil && playlistMeta != nil {
			revision = playlistMeta.GetRevision()
		}

		resolver, err := spclientpkg.NewContextResolver(ctx, p.app.log, p.sess.Spclient(), spotCtx)
		if err != nil {
			return nil, fmt.Errorf("failed creating playlist context resolver: %w", err)
		}

		jpegBytes, etag, err := p.resolvePlaylistGeneratedImage(ctx, resolver, spotId.Base62(), revision, data.Size)
		if err != nil {
			p.app.log.WithError(err).WithField("playlist_id", spotId.Base62()).Debug("failed generating playlist mosaic image")
			return nil, ErrNotFound
		}

		if data.IfNoneMatch != "" && etag != "" && data.IfNoneMatch == etag {
			return &ApiResponseBinary{
				StatusCode:   http.StatusNotModified,
				ContentType:  "image/jpeg",
				CacheControl: playlistMosaicCacheControl,
				ETag:         etag,
			}, nil
		}

		return &ApiResponseBinary{
			ContentType:  "image/jpeg",
			CacheControl: playlistMosaicCacheControl,
			ETag:         etag,
			Body:         jpegBytes,
		}, nil
	case ApiRequestTypeGetImage:
		data := req.Data.(ApiRequestDataGetImage)
		return p.resolveImageProxyBinary(ctx, data)
	case ApiRequestTypeGetRootlist:
		items, err := p.resolveRootlistMercury(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed resolving rootlist: %w", err)
		}
		if items == nil {
			items = []ApiResponseRootlistItem{}
		}

		data, _ := req.Data.(ApiRequestDataGetRootlist)
		resp := &ApiResponseRootlist{Playlists: items}
		if data.Paginate {
			total := len(items)
			offset := data.Offset
			limit := data.Limit
			resp.Playlists = pageRootlistItems(items, offset, limit)
			resp.Total = &total
			resp.Offset = &offset
			resp.Limit = &limit
		}

		return resp, nil
	case ApiRequestTypeGetQueue:
		imageSize := p.app.cfg.Server.ImageSize
		cache := newQueueEnrichmentCache()

		resp := &ApiResponseQueue{}
		if p.primaryStream != nil {
			current := queueTrackFromProvidedTrack(p.state.player.Track)
			current = p.enrichQueueTrack(ctx, current, imageSize, cache)
			resp.Current = &current
		}
		resp.PrevTracks = p.enrichQueueTracks(ctx, queueTracksFromProvidedTracks(p.state.player.PrevTracks), imageSize, cache)
		resp.NextTracks = p.enrichQueueTracks(ctx, queueTracksFromProvidedTracks(p.state.player.NextTracks), imageSize, cache)
		return resp, nil
	case ApiRequestTypeGetContext:
		data := req.Data.(ApiRequestDataGetMetadata)
		spotCtx, err := p.sess.Spclient().ContextResolve(ctx, data.Id)
		if err != nil {
			return nil, fmt.Errorf("failed resolving context: %w", err)
		}
		resp := &ApiResponseContext{Uri: spotCtx.Uri}
		for _, page := range spotCtx.Pages {
			for _, track := range page.Tracks {
				resp.Tracks = append(resp.Tracks, ApiResponseContextTrack{
					Uri:      track.Uri,
					Metadata: track.Metadata,
				})
			}
		}
		return resp, nil
	case ApiRequestTypeGetCollection:
		spotCtx, err := p.resolveUserContext(ctx, "collection")
		if err != nil {
			return nil, fmt.Errorf("failed resolving collection: %w", err)
		}
		var items []ApiResponseCollectionItem
		for _, page := range spotCtx.Pages {
			for _, track := range page.Tracks {
				items = append(items, ApiResponseCollectionItem{Uri: track.Uri})
			}
		}
		return &ApiResponseCollection{Items: items}, nil
	case ApiRequestTypeRadio:
		data := req.Data.(ApiRequestDataRadio)
		spotCtx, err := p.sess.Spclient().ContextResolveAutoplay(ctx, &playerpb.AutoplayContextRequest{
			ContextUri:     proto.String(data.SeedUri),
			RecentTrackUri: []string{data.SeedUri},
		})
		if err != nil {
			return nil, fmt.Errorf("failed resolving radio station: %w", err)
		}

		p.state.setActive(true)
		p.state.setPaused(false)
		p.state.player.Suppressions = &connectpb.Suppressions{}
		p.state.player.PlayOrigin = &connectpb.PlayOrigin{
			FeatureIdentifier: "go-librespot",
			FeatureVersion:    librespot.VersionNumberString(),
		}

		if err := p.loadContext(ctx, spotCtx, nil, false, true); err != nil {
			return nil, fmt.Errorf("failed loading radio context: %w", err)
		}

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown request type: %s", req.Type)
	}
}

func pointer[T any](d T) *T {
	return &d
}

func (p *AppPlayer) handleMprisEvent(ctx context.Context, req mpris.MediaPlayer2PlayerCommand) error {
	// Limit ourselves to 30 seconds for handling mpris commands
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch req.Type {
	case mpris.MediaPlayer2PlayerCommandTypeNext:
		return p.skipNext(ctx, nil)
	case mpris.MediaPlayer2PlayerCommandTypePrevious:
		return p.skipPrev(ctx, true)
	case mpris.MediaPlayer2PlayerCommandTypePlay:
		return p.play(ctx)
	case mpris.MediaPlayer2PlayerCommandTypePause:
		return p.pause(ctx)
	case mpris.MediaPlayer2PlayerCommandTypePlayPause:
		if p.state.player.IsPaused {
			return p.play(ctx)
		} else {
			return p.pause(ctx)
		}
	case mpris.MediaPlayer2PlayerCommandTypeStop:
		return p.stopPlayback(ctx)
	case mpris.MediaPlayer2PlayerCommandLoopStatusChanged:
		p.app.log.Tracef("mpris loop status argument %s", req.Argument)
		dt := req.Argument
		switch dt {
		case mpris.None:
			p.setOptions(ctx, pointer(false), pointer(false), nil)
		case mpris.Playlist:
			p.setOptions(ctx, pointer(true), pointer(false), nil)
		case mpris.Track:
			p.setOptions(ctx, pointer(true), pointer(true), nil)
		default:
			p.app.log.Warnf("mpris loop status argument is invalid (%s)", req.Argument)
		}
		return nil
	case mpris.MediaPlayer2PlayerCommandShuffleChanged:
		sh := req.Argument.(bool)
		p.setOptions(ctx, nil, nil, &sh)
		return nil
	case mpris.MediaPlayer2PlayerCommandVolumeChanged:
		volRelative := req.Argument.(float64)
		volAbs := uint32(player.MaxStateVolume * volRelative)

		p.updateVolume(volAbs)
		return nil
	case mpris.MediaPlayer2PlayerCommandTypeSetPosition:
		arg := req.Argument.(mpris.MediaPlayer2CommandSetPositionPayload)

		p.app.log.Tracef("media player set position argument: %v", arg)

		if arg.ObjectPath.IsValid() {
			spotifyId := strings.Join(strings.Split(string(arg.ObjectPath), "/")[3:], ":")
			if spotifyId != p.state.player.Track.Uri {
				return fmt.Errorf("seek tries to jump to different uri, not yet supported (got: %s, expected: %s)", spotifyId, p.state.player.Track.Uri)
			}
		}

		newPositionAbs := arg.PositionUs / 1000
		return p.seek(ctx, newPositionAbs)
	case mpris.MediaPlayer2PlayerCommandTypeSeek:
		newPosAbs := p.player.PositionMs() + req.Argument.(int64)/1000
		return p.seek(ctx, newPosAbs)
	case mpris.MediaPlayer2PlayerCommandTypeOpenUri, mpris.MediaPlayer2PlayerCommandRateChanged:
		p.app.log.Warnf("unimplemented mpris event %d", req.Type)
		return fmt.Errorf("unimplemented mpris event %d", req.Type)
	}
	return nil
}

func (p *AppPlayer) Close() {
	p.stop <- struct{}{}
	p.player.Close()
	p.sess.Close()
}

func (p *AppPlayer) Run(ctx context.Context, apiRecv <-chan ApiRequest, mprisRecv <-chan mpris.MediaPlayer2PlayerCommand) {
	err := p.sess.Dealer().Connect(ctx)
	if err != nil {
		p.app.log.WithError(err).Error("failed connecting to dealer")
		p.Close()
		return
	}

	apRecv := p.sess.Accesspoint().Receive(ap.PacketTypeProductInfo, ap.PacketTypeCountryCode)
	msgRecv := p.sess.Dealer().ReceiveMessage("hm://pusher/v1/connections/", "hm://connect-state/v1/")
	reqRecv := p.sess.Dealer().ReceiveRequest("hm://connect-state/v1/player/command")
	playerRecv := p.player.Receive()

	volumeTimer := time.NewTimer(time.Minute)
	volumeTimer.Stop() // don't emit a volume change event at start

	for {
		select {
		case <-p.stop:
			return
		case pkt, ok := <-apRecv:
			if !ok {
				continue
			}

			if err := p.handleAccesspointPacket(pkt.Type, pkt.Payload); err != nil {
				p.app.log.WithError(err).Warn("failed handling accesspoint packet")
			}
		case msg, ok := <-msgRecv:
			if !ok {
				continue
			}

			if err := p.handleDealerMessage(ctx, msg); err != nil {
				p.app.log.WithError(err).Warn("failed handling dealer message")
			}
		case req, ok := <-reqRecv:
			if !ok {
				continue
			}

			if err := p.handleDealerRequest(ctx, req); err != nil {
				p.app.log.WithError(err).Warn("failed handling dealer request")
				req.Reply(false)
			} else {
				p.app.log.Debugf("sending successful reply for dealer request")
				req.Reply(true)
			}
		case req, ok := <-apiRecv:
			if !ok {
				continue
			}

			data, err := p.handleApiRequest(ctx, req)
			req.Reply(data, err)
		case mprisReq, ok := <-mprisRecv:
			if !ok {
				continue
			}

			p.app.log.Tracef("new mpris message %v", mprisReq)
			err := p.handleMprisEvent(ctx, mprisReq)
			dbusError := mpris.MediaPlayer2PlayerCommandResponse{
				Err: &dbus.Error{},
			}
			if err != nil {
				dbusError.Err.Name = err.Error()
			} else {
				dbusError.Err = nil
			}
			mprisReq.Reply(dbusError)
		case ev, ok := <-playerRecv:
			if !ok {
				continue
			}

			p.handlePlayerEvent(ctx, &ev)
		case <-p.prefetchTimer.C:
			p.prefetchNext(ctx)
		case volume := <-p.volumeUpdate:
			// Received a new volume: from Spotify Connect, from the REST API,
			// or from the system volume mixer.
			// Because these updates can be quite frequent, we have to rate
			// limit them (otherwise we get HTTP error 429: Too many requests
			// for user).
			p.state.device.Volume = uint32(math.Round(float64(volume * player.MaxStateVolume)))
			volumeTimer.Reset(100 * time.Millisecond)
		case <-volumeTimer.C:
			// We've gone some time without update, send the new value now.
			p.volumeUpdated(ctx)
		}
	}
}

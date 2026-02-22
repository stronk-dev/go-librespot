package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	librespot "github.com/devgianlu/go-librespot"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/rs/cors"
)

const (
	timeout                  = 10 * time.Second
	spotifyIDMinLen          = 21
	spotifyIDMaxLen          = 22
	rootlistPageLimitDefault = 50
	playlistImageSizeDefault = 300
)

type ApiServer interface {
	Emit(ev *ApiEvent)
	Receive() <-chan ApiRequest
}

type ConcreteApiServer struct {
	log librespot.Logger

	allowOrigin string
	certFile    string
	keyFile     string

	close    bool
	listener net.Listener

	requests chan ApiRequest
	devApi   *DevApiTokenProvider

	clients     []*websocket.Conn
	clientsLock sync.RWMutex
}

var (
	ErrNoSession        = errors.New("no session")
	ErrBadRequest       = errors.New("bad request")
	ErrForbidden        = errors.New("forbidden")
	ErrNotFound         = errors.New("not found")
	ErrMethodNotAllowed = errors.New("method not allowed")
	ErrTooManyRequests  = errors.New("the app has exceeded its rate limits")
)

type ApiRequestType string

const (
	ApiRequestTypeWebApi              ApiRequestType = "web_api"
	ApiRequestTypeStatus              ApiRequestType = "status"
	ApiRequestTypeResume              ApiRequestType = "resume"
	ApiRequestTypePause               ApiRequestType = "pause"
	ApiRequestTypePlayPause           ApiRequestType = "playpause"
	ApiRequestTypeSeek                ApiRequestType = "seek"
	ApiRequestTypePrev                ApiRequestType = "prev"
	ApiRequestTypeNext                ApiRequestType = "next"
	ApiRequestTypePlay                ApiRequestType = "play"
	ApiRequestTypeGetVolume           ApiRequestType = "get_volume"
	ApiRequestTypeSetVolume           ApiRequestType = "set_volume"
	ApiRequestTypeSetRepeatingContext ApiRequestType = "repeating_context"
	ApiRequestTypeSetRepeatingTrack   ApiRequestType = "repeating_track"
	ApiRequestTypeSetShufflingContext ApiRequestType = "shuffling_context"
	ApiRequestTypeAddToQueue          ApiRequestType = "add_to_queue"
	ApiRequestTypeToken               ApiRequestType = "token"
	ApiRequestTypeGetTrack            ApiRequestType = "get_track"
	ApiRequestTypeGetAlbum            ApiRequestType = "get_album"
	ApiRequestTypeGetArtist           ApiRequestType = "get_artist"
	ApiRequestTypeGetShow             ApiRequestType = "get_show"
	ApiRequestTypeGetEpisode          ApiRequestType = "get_episode"
	ApiRequestTypeGetPlaylist         ApiRequestType = "get_playlist"
	ApiRequestTypeGetPlaylistImage    ApiRequestType = "get_playlist_image"
	ApiRequestTypeGetImage            ApiRequestType = "get_image"
	ApiRequestTypeGetRootlist         ApiRequestType = "get_rootlist"
	ApiRequestTypeGetQueue            ApiRequestType = "get_queue"
	ApiRequestTypeGetContext          ApiRequestType = "get_context"
	ApiRequestTypeRadio               ApiRequestType = "radio"
	ApiRequestTypeGetCollection       ApiRequestType = "get_collection"
)

type ApiEventType string

const (
	ApiEventTypePlaying        ApiEventType = "playing"
	ApiEventTypeNotPlaying     ApiEventType = "not_playing"
	ApiEventTypeWillPlay       ApiEventType = "will_play"
	ApiEventTypePaused         ApiEventType = "paused"
	ApiEventTypeActive         ApiEventType = "active"
	ApiEventTypeInactive       ApiEventType = "inactive"
	ApiEventTypeMetadata       ApiEventType = "metadata"
	ApiEventTypeVolume         ApiEventType = "volume"
	ApiEventTypeSeek           ApiEventType = "seek"
	ApiEventTypeStopped        ApiEventType = "stopped"
	ApiEventTypeRepeatTrack    ApiEventType = "repeat_track"
	ApiEventTypeRepeatContext  ApiEventType = "repeat_context"
	ApiEventTypeShuffleContext ApiEventType = "shuffle_context"
	ApiEventTypeQueue          ApiEventType = "queue"
	ApiEventTypeContext        ApiEventType = "context"
)

type ApiRequest struct {
	Type ApiRequestType
	Data any

	resp chan apiResponse
}

func (r *ApiRequest) Reply(data any, err error) {
	r.resp <- apiResponse{data, err}
}

type ApiRequestDataSeek struct {
	Position int64 `json:"position"`
	Relative bool  `json:"relative"`
}

type ApiRequestDataVolume struct {
	Volume   int32 `json:"volume"`
	Relative bool  `json:"relative"`
}

type ApiRequestDataWebApi struct {
	Method string
	Path   string
	Query  url.Values
}

type ApiRequestDataPlay struct {
	Uri       string `json:"uri"`
	SkipToUri string `json:"skip_to_uri"`
	Paused    bool   `json:"paused"`
}

type ApiRequestDataGetMetadata struct {
	Id        string
	ImageSize string
	Limit     int
	Offset    int
}

type ApiRequestDataGetRootlist struct {
	Limit    int
	Offset   int
	Paginate bool
}

type ApiRequestDataGetPlaylistImage struct {
	Id          string
	Size        int
	IfNoneMatch string
}

type ApiRequestDataGetImage struct {
	Id          string
	IfNoneMatch string
}

type ApiRequestDataNext struct {
	Uri *string `json:"uri"`
}

type ApiRequestDataRadio struct {
	SeedUri string `json:"seed_uri"`
}

type apiResponse struct {
	data any
	err  error
}

type ApiResponseStatusTrack struct {
	Uri           string           `json:"uri"`
	Name          string           `json:"name"`
	ArtistNames   []string         `json:"artist_names"`
	AlbumName     string           `json:"album_name"`
	AlbumCoverUrl *string          `json:"album_cover_url"`
	AlbumUri      string           `json:"album_uri,omitempty"`
	Artists       []ApiResponseRef `json:"artists,omitempty"`
	Position      int64            `json:"position"`
	Duration      int              `json:"duration"`
	ReleaseDate   string           `json:"release_date"`
	TrackNumber   int              `json:"track_number"`
	DiscNumber    int              `json:"disc_number"`
}

func getBestImageIdForSize(images []*metadatapb.Image, size string) []byte {
	if len(images) == 0 {
		return nil
	}

	imageSize := metadatapb.Image_Size(metadatapb.Image_Size_value[strings.ToUpper(size)])

	dist := func(a metadatapb.Image_Size) int {
		diff := int(a) - int(imageSize)
		if diff < 0 {
			return -diff
		}
		return diff
	}

	// Find an image with the exact requested size.
	// If no exact match, return the closest image to the requested size.
	var bestImage *metadatapb.Image
	for _, img := range images {
		if img.Size == nil {
			continue
		}

		if *img.Size == imageSize {
			return img.FileId
		}

		// Find the image with the closest size. This logic works because the
		// metadatapb.Image_Size enum values are ordered from smallest to largest.
		if bestImage == nil || dist(*img.Size) < dist(*bestImage.Size) {
			bestImage = img
		}
	}

	if bestImage != nil {
		return bestImage.FileId
	}

	// Fallback to the first image if none have size information.
	return images[0].FileId
}

func (p *AppPlayer) newApiResponseStatusTrack(media *librespot.Media, position int64) *ApiResponseStatusTrack {
	if media.IsTrack() {
		track := media.Track()
		if track == nil {
			return &ApiResponseStatusTrack{Position: position, ArtistNames: []string{}}
		}

		artistNames := make([]string, 0, len(track.Artist))
		artistRefs := make([]ApiResponseRef, 0, len(track.Artist))
		for _, a := range track.Artist {
			if a == nil {
				continue
			}
			if name := a.GetName(); name != "" {
				artistNames = append(artistNames, name)
			}
			artistRefs = append(artistRefs, artistRef(a))
		}

		var (
			albumName     string
			albumCoverUrl *string
			albumUri      string
			releaseDate   string
		)
		if track.Album != nil {
			albumName = track.Album.GetName()
			albumCoverUrl = p.resolveImageUrl(track.Album.Cover, track.Album.CoverGroup, p.app.cfg.Server.ImageSize)
			albumUri = spotifyURIFromGID(librespot.SpotifyIdTypeAlbum, track.Album.Gid)
			if track.Album.Date != nil {
				releaseDate = track.Album.Date.String()
			}
		}

		return &ApiResponseStatusTrack{
			Uri:           spotifyURIFromGID(librespot.SpotifyIdTypeTrack, track.Gid),
			Name:          track.GetName(),
			ArtistNames:   artistNames,
			AlbumName:     albumName,
			AlbumCoverUrl: albumCoverUrl,
			AlbumUri:      albumUri,
			Artists:       artistRefs,
			Position:      position,
			Duration:      int(track.GetDuration()),
			ReleaseDate:   releaseDate,
			TrackNumber:   int(track.GetNumber()),
			DiscNumber:    int(track.GetDiscNumber()),
		}
	} else {
		episode := media.Episode()
		if episode == nil {
			return &ApiResponseStatusTrack{Position: position, ArtistNames: []string{}}
		}

		showName := ""
		showUri := ""
		artists := make([]ApiResponseRef, 0, 1)
		artistNames := make([]string, 0, 1)
		if episode.Show != nil {
			showName = episode.Show.GetName()
			showUri = spotifyURIFromGID(librespot.SpotifyIdTypeShow, episode.Show.Gid)
			if showName != "" || showUri != "" {
				artists = append(artists, ApiResponseRef{Uri: showUri, Name: showName})
			}
			if showName != "" {
				artistNames = append(artistNames, showName)
			}
		}

		resp := &ApiResponseStatusTrack{
			Uri:           spotifyURIFromGID(librespot.SpotifyIdTypeEpisode, episode.Gid),
			Name:          episode.GetName(),
			ArtistNames:   artistNames,
			AlbumName:     showName,
			AlbumCoverUrl: p.resolveImageUrl(nil, episode.CoverImage, p.app.cfg.Server.ImageSize),
			Position:      position,
			Duration:      int(episode.GetDuration()),
		}

		if len(artists) > 0 {
			resp.Artists = artists
		}

		return resp
	}
}

func (p *AppPlayer) resolveImageUrl(images []*metadatapb.Image, imageGroup *metadatapb.ImageGroup, imageSize string) *string {
	fileId := getBestImageIdForSize(images, imageSize)
	if fileId == nil && imageGroup != nil {
		fileId = getBestImageIdForSize(imageGroup.Image, imageSize)
	}
	if fileId == nil {
		return nil
	}

	if localPath := localImagePathFromFileID(fileId); localPath != "" {
		return &localPath
	}
	if p.prodInfo == nil {
		return nil
	}

	return p.prodInfo.ImageUrl(fileId)
}

func spotifyURIFromGID(typ librespot.SpotifyIdType, gid []byte) string {
	if len(gid) != 16 {
		return ""
	}
	return librespot.SpotifyIdFromGid(typ, gid).Uri()
}

func artistRef(a *metadatapb.Artist) ApiResponseRef {
	if a == nil {
		return ApiResponseRef{}
	}
	return ApiResponseRef{
		Uri:  spotifyURIFromGID(librespot.SpotifyIdTypeArtist, a.Gid),
		Name: a.GetName(),
	}
}

func albumRef(a *metadatapb.Album) ApiResponseRef {
	if a == nil {
		return ApiResponseRef{}
	}
	return ApiResponseRef{
		Uri:  spotifyURIFromGID(librespot.SpotifyIdTypeAlbum, a.Gid),
		Name: a.GetName(),
	}
}

func (p *AppPlayer) convertTrackProto(track *metadatapb.Track, imageSize string) *ApiResponseTrackFull {
	if track == nil {
		return &ApiResponseTrackFull{}
	}

	artists := make([]ApiResponseRef, 0, len(track.Artist))
	for _, a := range track.Artist {
		ref := artistRef(a)
		if ref.Uri != "" || ref.Name != "" {
			artists = append(artists, ref)
		}
	}

	resp := &ApiResponseTrackFull{
		Uri:     spotifyURIFromGID(librespot.SpotifyIdTypeTrack, track.Gid),
		Name:    track.GetName(),
		Artists: artists,
	}

	if track.Album != nil {
		ref := albumRef(track.Album)
		resp.Album = &ref
		resp.AlbumCoverUrl = p.resolveImageUrl(track.Album.Cover, track.Album.CoverGroup, imageSize)
		if track.Album.Date != nil {
			resp.ReleaseDate = track.Album.Date.String()
		}
	}

	resp.Duration = int(track.GetDuration())
	resp.TrackNumber = int(track.GetNumber())
	resp.DiscNumber = int(track.GetDiscNumber())
	resp.Popularity = int(track.GetPopularity())
	resp.Explicit = track.GetExplicit()

	return resp
}

func (p *AppPlayer) convertAlbumProto(album *metadatapb.Album, imageSize string) *ApiResponseAlbumFull {
	if album == nil {
		return &ApiResponseAlbumFull{}
	}

	artists := make([]ApiResponseRef, 0, len(album.Artist))
	for _, a := range album.Artist {
		ref := artistRef(a)
		if ref.Uri != "" || ref.Name != "" {
			artists = append(artists, ref)
		}
	}

	resp := &ApiResponseAlbumFull{
		Uri:      spotifyURIFromGID(librespot.SpotifyIdTypeAlbum, album.Gid),
		Name:     album.GetName(),
		Artists:  artists,
		Label:    album.GetLabel(),
		CoverUrl: p.resolveImageUrl(album.Cover, album.CoverGroup, imageSize),
	}

	if album.Type != nil {
		resp.Type = strings.ToLower(album.Type.String())
	}
	if album.Date != nil {
		resp.ReleaseDate = album.Date.String()
	}
	resp.Popularity = int(album.GetPopularity())

	// Flatten disc tracks
	for _, disc := range album.Disc {
		if disc == nil {
			continue
		}
		for _, track := range disc.Track {
			resp.Tracks = append(resp.Tracks, *p.convertTrackProto(track, imageSize))
		}
	}
	resp.TotalTracks = len(resp.Tracks)

	return resp
}

func (p *AppPlayer) convertArtistProto(artist *metadatapb.Artist, imageSize string, countryCode string) *ApiResponseArtistFull {
	if artist == nil {
		return &ApiResponseArtistFull{}
	}

	resp := &ApiResponseArtistFull{
		Uri:         spotifyURIFromGID(librespot.SpotifyIdTypeArtist, artist.Gid),
		Name:        artist.GetName(),
		PortraitUrl: p.resolveImageUrl(artist.Portrait, artist.PortraitGroup, imageSize),
	}

	resp.Popularity = int(artist.GetPopularity())

	if len(artist.Biography) > 0 {
		resp.Biography = artist.Biography[0].GetText()
	}

	// Top tracks — filter by country code, fall back to first available
	for _, tt := range artist.TopTrack {
		if tt != nil && tt.Country != nil && *tt.Country == countryCode {
			for _, track := range tt.Track {
				resp.TopTracks = append(resp.TopTracks, *p.convertTrackProto(track, imageSize))
			}
			break
		}
	}
	if len(resp.TopTracks) == 0 && len(artist.TopTrack) > 0 {
		for _, track := range artist.TopTrack[0].Track {
			resp.TopTracks = append(resp.TopTracks, *p.convertTrackProto(track, imageSize))
		}
	}

	// Albums
	for _, group := range artist.AlbumGroup {
		if group == nil {
			continue
		}
		for _, a := range group.Album {
			ref := albumRef(a)
			if ref.Uri != "" || ref.Name != "" {
				resp.Albums = append(resp.Albums, ref)
			}
		}
	}

	// Singles
	for _, group := range artist.SingleGroup {
		if group == nil {
			continue
		}
		for _, a := range group.Album {
			ref := albumRef(a)
			if ref.Uri != "" || ref.Name != "" {
				resp.Singles = append(resp.Singles, ref)
			}
		}
	}

	// Related artists
	for _, r := range artist.Related {
		ref := artistRef(r)
		if ref.Uri != "" || ref.Name != "" {
			resp.Related = append(resp.Related, ref)
		}
	}

	return resp
}

func (p *AppPlayer) convertShowProto(show *metadatapb.Show, imageSize string) *ApiResponseShowFull {
	if show == nil {
		return &ApiResponseShowFull{}
	}

	resp := &ApiResponseShowFull{
		Uri:         spotifyURIFromGID(librespot.SpotifyIdTypeShow, show.Gid),
		Name:        show.GetName(),
		Description: show.GetDescription(),
		Publisher:   show.GetPublisher(),
		CoverUrl:    p.resolveImageUrl(nil, show.CoverImage, imageSize),
	}

	if show.MediaType != nil {
		resp.MediaType = strings.ToLower(show.MediaType.String())
	}

	for _, ep := range show.Episode {
		if ep != nil {
			uri := spotifyURIFromGID(librespot.SpotifyIdTypeEpisode, ep.Gid)
			if uri == "" {
				continue
			}
			resp.Episodes = append(resp.Episodes, ApiResponseRef{
				Uri:  uri,
				Name: ep.GetName(),
			})
		}
	}

	return resp
}

func (p *AppPlayer) convertEpisodeProto(episode *metadatapb.Episode, imageSize string) *ApiResponseEpisodeFull {
	if episode == nil {
		return &ApiResponseEpisodeFull{}
	}

	resp := &ApiResponseEpisodeFull{
		Uri:         spotifyURIFromGID(librespot.SpotifyIdTypeEpisode, episode.Gid),
		Name:        episode.GetName(),
		Description: episode.GetDescription(),
		CoverUrl:    p.resolveImageUrl(nil, episode.CoverImage, imageSize),
		Explicit:    episode.GetExplicit(),
	}

	resp.Duration = int(episode.GetDuration())

	if episode.Show != nil {
		ref := ApiResponseRef{
			Name: episode.Show.GetName(),
		}
		ref.Uri = spotifyURIFromGID(librespot.SpotifyIdTypeShow, episode.Show.Gid)
		resp.Show = &ref
	}

	if episode.PublishTime != nil {
		resp.PublishDate = episode.PublishTime.String()
	}

	return resp
}

type ApiResponseStatus struct {
	Username       string                  `json:"username"`
	DeviceId       string                  `json:"device_id"`
	DeviceType     string                  `json:"device_type"`
	DeviceName     string                  `json:"device_name"`
	PlayOrigin     string                  `json:"play_origin"`
	Stopped        bool                    `json:"stopped"`
	Paused         bool                    `json:"paused"`
	Buffering      bool                    `json:"buffering"`
	Volume         uint32                  `json:"volume"`
	VolumeSteps    uint32                  `json:"volume_steps"`
	RepeatContext  bool                    `json:"repeat_context"`
	RepeatTrack    bool                    `json:"repeat_track"`
	ShuffleContext bool                    `json:"shuffle_context"`
	Track          *ApiResponseStatusTrack `json:"track"`
}

type ApiResponseVolume struct {
	Value uint32 `json:"value"`
	Max   uint32 `json:"max"`
}

type ApiResponseToken struct {
	Token string `json:"token"`
}

// ApiResponseWebApi is a transparent proxy response from the Spotify Web API.
// Instead of parsing and re-encoding, the raw response is passed through.
type ApiResponseWebApi struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type ApiResponseBinary struct {
	StatusCode   int
	ContentType  string
	CacheControl string
	ETag         string
	Body         []byte
}

type ApiResponseRef struct {
	Uri  string `json:"uri"`
	Name string `json:"name"`
}

type ApiResponseTrackFull struct {
	Uri           string           `json:"uri"`
	Name          string           `json:"name"`
	Artists       []ApiResponseRef `json:"artists"`
	Album         *ApiResponseRef  `json:"album,omitempty"`
	AlbumCoverUrl *string          `json:"album_cover_url,omitempty"`
	Duration      int              `json:"duration"`
	TrackNumber   int              `json:"track_number"`
	DiscNumber    int              `json:"disc_number"`
	Popularity    int              `json:"popularity"`
	Explicit      bool             `json:"explicit"`
	ReleaseDate   string           `json:"release_date,omitempty"`
}

type ApiResponseAlbumFull struct {
	Uri         string                 `json:"uri"`
	Name        string                 `json:"name"`
	Artists     []ApiResponseRef       `json:"artists"`
	Type        string                 `json:"type"`
	Label       string                 `json:"label,omitempty"`
	ReleaseDate string                 `json:"release_date,omitempty"`
	CoverUrl    *string                `json:"cover_url,omitempty"`
	Popularity  int                    `json:"popularity"`
	TotalTracks int                    `json:"total_tracks"`
	Tracks      []ApiResponseTrackFull `json:"tracks"`
}

type ApiResponseArtistFull struct {
	Uri         string                 `json:"uri"`
	Name        string                 `json:"name"`
	PortraitUrl *string                `json:"portrait_url,omitempty"`
	Popularity  int                    `json:"popularity"`
	Biography   string                 `json:"biography,omitempty"`
	TopTracks   []ApiResponseTrackFull `json:"top_tracks"`
	Albums      []ApiResponseRef       `json:"albums"`
	Singles     []ApiResponseRef       `json:"singles"`
	Related     []ApiResponseRef       `json:"related"`
}

type ApiResponseShowFull struct {
	Uri         string           `json:"uri"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Publisher   string           `json:"publisher,omitempty"`
	CoverUrl    *string          `json:"cover_url,omitempty"`
	MediaType   string           `json:"media_type,omitempty"`
	Episodes    []ApiResponseRef `json:"episodes"`
}

type ApiResponseEpisodeFull struct {
	Uri         string          `json:"uri"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Duration    int             `json:"duration"`
	CoverUrl    *string         `json:"cover_url,omitempty"`
	Show        *ApiResponseRef `json:"show,omitempty"`
	PublishDate string          `json:"publish_date,omitempty"`
	Explicit    bool            `json:"explicit"`
}

type ApiResponsePlaylistItem struct {
	Uri     string `json:"uri"`
	AddedBy string `json:"added_by,omitempty"`
	AddedAt int64  `json:"added_at,omitempty"`
}

type ApiResponsePlaylistFull struct {
	Uri              string                    `json:"uri"`
	Name             string                    `json:"name"`
	Description      string                    `json:"description,omitempty"`
	OwnerUsername    string                    `json:"owner_username,omitempty"`
	OwnerDisplayName string                    `json:"owner_display_name,omitempty"`
	Collaborative    bool                      `json:"collaborative"`
	TotalTracks      int                       `json:"total_tracks"`
	ImageUrl         *string                   `json:"image_url,omitempty"`
	Items            []ApiResponsePlaylistItem `json:"items"`
}

type ApiResponseRootlistItem struct {
	Uri  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

type ApiResponseRootlist struct {
	Playlists []ApiResponseRootlistItem `json:"playlists"`
	Total     *int                      `json:"total,omitempty"`
	Offset    *int                      `json:"offset,omitempty"`
	Limit     *int                      `json:"limit,omitempty"`
}

type ApiResponseCollectionItem struct {
	Uri string `json:"uri"`
}

type ApiResponseCollection struct {
	Items []ApiResponseCollectionItem `json:"items"`
}

type ApiResponseQueueTrack struct {
	Uri      string                  `json:"uri"`
	Name     string                  `json:"name,omitempty"`
	Provider string                  `json:"provider,omitempty"`
	Track    *ApiResponseTrackFull   `json:"track,omitempty"`
	Episode  *ApiResponseEpisodeFull `json:"episode,omitempty"`
}

type ApiResponseQueue struct {
	Current    *ApiResponseQueueTrack  `json:"current,omitempty"`
	PrevTracks []ApiResponseQueueTrack `json:"prev_tracks"`
	NextTracks []ApiResponseQueueTrack `json:"next_tracks"`
}

type ApiResponseContextTrack struct {
	Uri      string            `json:"uri"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type ApiResponseContext struct {
	Uri    string                    `json:"uri"`
	Tracks []ApiResponseContextTrack `json:"tracks"`
}

type ApiEventDataQueue struct {
	PrevTracks []ApiResponseQueueTrack `json:"prev_tracks"`
	NextTracks []ApiResponseQueueTrack `json:"next_tracks"`
}

type ApiEventDataContext struct {
	ContextUri string `json:"context_uri"`
}

type ApiEvent struct {
	Type ApiEventType `json:"type"`
	Data any          `json:"data"`
}

type ApiEventDataMetadata ApiResponseStatusTrack

type ApiEventDataVolume ApiResponseVolume

type ApiEventDataPlaying struct {
	ContextUri string `json:"context_uri"`
	Uri        string `json:"uri"`
	Resume     bool   `json:"resume"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataWillPlay struct {
	ContextUri string `json:"context_uri"`
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataNotPlaying struct {
	ContextUri string `json:"context_uri"`
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataPaused struct {
	ContextUri string `json:"context_uri"`
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataStopped struct {
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataSeek struct {
	ContextUri string `json:"context_uri"`
	Uri        string `json:"uri"`
	Position   int    `json:"position"`
	Duration   int    `json:"duration"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataRepeatTrack struct {
	Value bool `json:"value"`
}

type ApiEventDataRepeatContext struct {
	Value bool `json:"value"`
}

type ApiEventDataShuffleContext struct {
	Value bool `json:"value"`
}

func NewApiServer(log librespot.Logger, address string, port int, allowOrigin string, certFile string, keyFile string, devApi *DevApiTokenProvider) (_ ApiServer, err error) {
	s := &ConcreteApiServer{log: log, allowOrigin: allowOrigin, certFile: certFile, keyFile: keyFile, devApi: devApi}
	s.requests = make(chan ApiRequest)

	s.listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return nil, fmt.Errorf("failed starting api listener: %w", err)
	}

	log.Infof("api server listening on %s", s.listener.Addr())

	go s.serve()
	return s, nil
}

type StubApiServer struct {
	log librespot.Logger
}

func NewStubApiServer(log librespot.Logger) (ApiServer, error) {
	return &StubApiServer{log: log}, nil
}

func (s *StubApiServer) Emit(ev *ApiEvent) {
	s.log.Tracef("voiding websocket event: %s", ev.Type)
}

func (s *StubApiServer) Receive() <-chan ApiRequest {
	return make(<-chan ApiRequest)
}

func (s *ConcreteApiServer) handleRequest(req ApiRequest, w http.ResponseWriter) {
	req.resp = make(chan apiResponse, 1)
	s.requests <- req
	resp := <-req.resp

	if resp.err != nil {
		switch {
		case errors.Is(resp.err, ErrNoSession):
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(resp.err, ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			return
		case errors.Is(resp.err, ErrNotFound):
			w.WriteHeader(http.StatusNotFound)
			return
		case errors.Is(resp.err, ErrMethodNotAllowed):
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case errors.Is(resp.err, ErrTooManyRequests):
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case errors.Is(resp.err, ErrBadRequest):
			w.WriteHeader(http.StatusBadRequest)
			return
		default:
			s.log.WithError(resp.err).Errorf("failed handling request %s", req.Type)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	switch respData := resp.data.(type) {
	case *ApiResponseWebApi:
		for k, v := range respData.Header {
			if strings.HasPrefix(k, "Content-") || k == "Retry-After" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(respData.StatusCode)
		_, _ = w.Write(respData.Body)
	case *ApiResponseBinary:
		if respData.ContentType != "" {
			w.Header().Set("Content-Type", respData.ContentType)
		}
		if respData.CacheControl != "" {
			w.Header().Set("Cache-Control", respData.CacheControl)
		}
		if respData.ETag != "" {
			w.Header().Set("ETag", respData.ETag)
		}
		if respData.StatusCode > 0 {
			w.WriteHeader(respData.StatusCode)
		}
		if len(respData.Body) > 0 {
			_, _ = w.Write(respData.Body)
		}
	case []byte:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(respData)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respData)
	}
}

func jsonDecode(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	} else if len(data) == 0 {
		return nil
	}

	return json.Unmarshal(data, v)
}

func parseIntQuery(query url.Values, key string, minValue int) (int, bool) {
	raw := query.Get(key)
	if raw == "" {
		return 0, true
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < minValue {
		return 0, false
	}

	return n, true
}

func isSpotifyBase62ID(id string) bool {
	if len(id) < spotifyIDMinLen || len(id) > spotifyIDMaxLen {
		return false
	}

	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}

	return true
}

func parseMetadataRequest(r *http.Request, pathPrefix string, withPaging bool) (ApiRequestDataGetMetadata, bool) {
	id := strings.TrimPrefix(r.URL.Path, pathPrefix)
	if !isSpotifyBase62ID(id) {
		return ApiRequestDataGetMetadata{}, false
	}

	data := ApiRequestDataGetMetadata{
		Id:        id,
		ImageSize: r.URL.Query().Get("image_size"),
	}

	if withPaging {
		limit, ok := parseIntQuery(r.URL.Query(), "limit", 1)
		if !ok {
			return ApiRequestDataGetMetadata{}, false
		}
		offset, ok := parseIntQuery(r.URL.Query(), "offset", 0)
		if !ok {
			return ApiRequestDataGetMetadata{}, false
		}
		data.Limit = limit
		data.Offset = offset
	}

	return data, true
}

func parsePlaylistImageRequest(r *http.Request, pathPrefix string) (ApiRequestDataGetPlaylistImage, bool) {
	raw := strings.TrimPrefix(r.URL.Path, pathPrefix)
	if !strings.HasSuffix(raw, "/image") {
		return ApiRequestDataGetPlaylistImage{}, false
	}

	id := strings.TrimSuffix(raw, "/image")
	if !isSpotifyBase62ID(id) {
		return ApiRequestDataGetPlaylistImage{}, false
	}

	size := playlistImageSizeDefault
	if rawSize := strings.TrimSpace(r.URL.Query().Get("size")); rawSize != "" {
		n, err := strconv.Atoi(rawSize)
		if err != nil {
			return ApiRequestDataGetPlaylistImage{}, false
		}
		switch n {
		case 60, 300, 640:
			size = n
		default:
			return ApiRequestDataGetPlaylistImage{}, false
		}
	}

	return ApiRequestDataGetPlaylistImage{
		Id:          id,
		Size:        size,
		IfNoneMatch: strings.TrimSpace(r.Header.Get("If-None-Match")),
	}, true
}

func parseImageProxyRequest(r *http.Request, pathPrefix string) (ApiRequestDataGetImage, bool) {
	id := strings.TrimPrefix(r.URL.Path, pathPrefix)
	if !isHexImageID(id) {
		return ApiRequestDataGetImage{}, false
	}

	return ApiRequestDataGetImage{
		Id:          strings.ToLower(id),
		IfNoneMatch: strings.TrimSpace(r.Header.Get("If-None-Match")),
	}, true
}

func parseRootlistRequest(r *http.Request) (ApiRequestDataGetRootlist, bool) {
	query := r.URL.Query()
	hasLimit := query.Get("limit") != ""
	hasOffset := query.Get("offset") != ""
	if !hasLimit && !hasOffset {
		return ApiRequestDataGetRootlist{}, true
	}

	data := ApiRequestDataGetRootlist{
		Paginate: true,
		Limit:    rootlistPageLimitDefault,
	}

	if hasLimit {
		limit, ok := parseIntQuery(query, "limit", 1)
		if !ok {
			return ApiRequestDataGetRootlist{}, false
		}
		data.Limit = limit
	}

	if hasOffset {
		offset, ok := parseIntQuery(query, "offset", 0)
		if !ok {
			return ApiRequestDataGetRootlist{}, false
		}
		data.Offset = offset
	}

	return data, true
}

func (s *ConcreteApiServer) serve() {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	m.Handle("/web-api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/web-api/")
		query := r.URL.Query()

		// If dev API is authorized, use it directly (separate rate limit bucket).
		if s.devApi != nil && s.devApi.IsAuthorized() {
			resp, err := s.devApi.WebApiRequest(r.Context(), r.Method, path, query)
			if err != nil {
				s.log.WithError(err).Error("dev API: web request failed, falling back to internal token")
			} else {
				defer func() { _ = resp.Body.Close() }()
				for k, v := range resp.Header {
					if strings.HasPrefix(k, "Content-") || k == "Retry-After" {
						w.Header()[k] = v
					}
				}
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
				return
			}
		}

		// Fall back to internal token via player channel.
		s.handleRequest(ApiRequest{
			Type: ApiRequestTypeWebApi,
			Data: ApiRequestDataWebApi{
				Method: r.Method,
				Path:   path,
				Query:  query,
			},
		}, w)
	}))
	m.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeStatus}, w)
	})
	m.HandleFunc("/player/play", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data ApiRequestDataPlay
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if len(data.Uri) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePlay, Data: data}, w)
	})
	m.HandleFunc("/player/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeResume}, w)
	})
	m.HandleFunc("/player/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePause}, w)
	})
	m.HandleFunc("/player/playpause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePlayPause}, w)
	})
	m.HandleFunc("/player/next", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data ApiRequestDataNext
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeNext, Data: data}, w)
	})
	m.HandleFunc("/player/prev", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePrev}, w)
	})
	m.HandleFunc("/player/seek", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data ApiRequestDataSeek
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if !data.Relative && data.Position < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSeek, Data: data}, w)
	})
	m.HandleFunc("/player/volume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			s.handleRequest(ApiRequest{Type: ApiRequestTypeGetVolume}, w)
		} else if r.Method == "POST" {
			var data ApiRequestDataVolume
			if err := jsonDecode(r, &data); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !data.Relative && data.Volume < 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			s.handleRequest(ApiRequest{Type: ApiRequestTypeSetVolume, Data: data}, w)
		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	m.HandleFunc("/player/repeat_context", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Repeat bool `json:"repeat_context"`
		}
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSetRepeatingContext, Data: data.Repeat}, w)
	})
	m.HandleFunc("/player/repeat_track", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Repeat bool `json:"repeat_track"`
		}
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSetRepeatingTrack, Data: data.Repeat}, w)
	})
	m.HandleFunc("/player/shuffle_context", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Shuffle bool `json:"shuffle_context"`
		}
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSetShufflingContext, Data: data.Shuffle}, w)
	})
	m.HandleFunc("/player/add_to_queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Uri string `json:"uri"`
		}
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if len(data.Uri) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeAddToQueue, Data: data.Uri}, w)
	})
	m.HandleFunc("/player/radio", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data ApiRequestDataRadio
		if err := jsonDecode(r, &data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if len(data.SeedUri) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeRadio, Data: data}, w)
	})
	registerMetadataHandler := func(pathPrefix string, reqType ApiRequestType, withPaging bool) {
		m.Handle(pathPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}

			data, ok := parseMetadataRequest(r, pathPrefix, withPaging)
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			s.handleRequest(ApiRequest{Type: reqType, Data: data}, w)
		}))
	}

	registerMetadataHandler("/metadata/track/", ApiRequestTypeGetTrack, false)
	registerMetadataHandler("/metadata/album/", ApiRequestTypeGetAlbum, false)
	registerMetadataHandler("/metadata/artist/", ApiRequestTypeGetArtist, false)
	registerMetadataHandler("/metadata/show/", ApiRequestTypeGetShow, false)
	registerMetadataHandler("/metadata/episode/", ApiRequestTypeGetEpisode, false)
	m.Handle("/metadata/playlist/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if strings.HasSuffix(r.URL.Path, "/image") {
			data, ok := parsePlaylistImageRequest(r, "/metadata/playlist/")
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.handleRequest(ApiRequest{Type: ApiRequestTypeGetPlaylistImage, Data: data}, w)
			return
		}

		data, ok := parseMetadataRequest(r, "/metadata/playlist/", true)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleRequest(ApiRequest{Type: ApiRequestTypeGetPlaylist, Data: data}, w)
	}))
	m.Handle("/image/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		data, ok := parseImageProxyRequest(r, "/image/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeGetImage, Data: data}, w)
	}))
	m.HandleFunc("/metadata/rootlist", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		data, ok := parseRootlistRequest(r)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleRequest(ApiRequest{Type: ApiRequestTypeGetRootlist, Data: data}, w)
	})
	m.HandleFunc("/metadata/collection", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.handleRequest(ApiRequest{Type: ApiRequestTypeGetCollection}, w)
	})
	m.HandleFunc("/player/queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.handleRequest(ApiRequest{Type: ApiRequestTypeGetQueue}, w)
	})
	m.Handle("/context/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		uri, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/context/"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(uri) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handleRequest(ApiRequest{
			Type: ApiRequestTypeGetContext,
			Data: ApiRequestDataGetMetadata{Id: uri},
		}, w)
	}))
	m.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeToken}, w)
	})
	m.HandleFunc("/devapi/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		configured := s.devApi != nil
		authorized := configured && s.devApi.IsAuthorized()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": configured,
			"authorized": authorized,
		})
	})
	m.HandleFunc("/devapi/authorize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if s.devApi == nil {
			http.Error(w, "dev API not configured", http.StatusBadRequest)
			return
		}

		http.Redirect(w, r, s.devApi.AuthorizeURL(), http.StatusFound)
	})
	m.HandleFunc("/devapi/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if s.devApi == nil {
			http.Error(w, "dev API not configured", http.StatusBadRequest)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error")
			http.Error(w, fmt.Sprintf("authorization failed: %s", errMsg), http.StatusBadRequest)
			return
		}

		if err := s.devApi.HandleCallback(r.Context(), code); err != nil {
			s.log.WithError(err).Error("failed handling dev API callback")
			http.Error(w, "token exchange failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Authorization successful</h1><p>You can close this window. go-librespot will now use your Developer API token for /web-api/ requests.</p></body></html>"))
	})
	m.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		opts := &websocket.AcceptOptions{}
		if len(s.allowOrigin) > 0 {
			allow := s.allowOrigin
			allow = strings.TrimPrefix(allow, "http://")
			allow = strings.TrimPrefix(allow, "https://")
			allow = strings.TrimSuffix(allow, "/")
			opts.OriginPatterns = []string{allow}
		}

		c, err := websocket.Accept(w, r, opts)
		if err != nil {
			s.log.WithError(err).Error("failed accepting websocket connection")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// add the client to the list
		s.clientsLock.Lock()
		s.clients = append(s.clients, c)
		s.clientsLock.Unlock()

		s.log.Debugf("new websocket client")

		for {
			_, _, err := c.Read(context.Background())
			if s.close {
				return
			} else if err != nil {
				s.log.WithError(err).Error("websocket connection errored")

				// remove the client from the list
				s.clientsLock.Lock()
				for i, cc := range s.clients {
					if cc == c {
						s.clients = append(s.clients[:i], s.clients[i+1:]...)
						break
					}
				}
				s.clientsLock.Unlock()
				return
			}
		}
	})

	c := cors.New(cors.Options{
		AllowedOrigins:      []string{s.allowOrigin},
		AllowPrivateNetwork: true,
		AllowCredentials:    true,
	})

	var err error
	if len(s.certFile) > 0 && len(s.keyFile) > 0 {
		err = http.ServeTLS(s.listener, c.Handler(m), s.certFile, s.keyFile)
	} else {
		err = http.Serve(s.listener, c.Handler(m))
	}

	if s.close {
		return
	} else if err != nil {
		s.log.WithError(err).Error("failed serving api")
		s.Close()
	}
}

func (s *ConcreteApiServer) Emit(ev *ApiEvent) {
	s.clientsLock.RLock()
	defer s.clientsLock.RUnlock()

	s.log.Tracef("emitting websocket event: %s", ev.Type)

	for _, client := range s.clients {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := wsjson.Write(ctx, client, ev)
		cancel()
		if err != nil {
			// purposely do not propagate this to the caller
			s.log.WithError(err).Error("failed communicating with websocket client")
		}
	}
}

func (s *ConcreteApiServer) Receive() <-chan ApiRequest {
	return s.requests
}

func (s *ConcreteApiServer) Close() {
	s.close = true

	// close all websocket clients
	s.clientsLock.RLock()
	for _, client := range s.clients {
		_ = client.Close(websocket.StatusGoingAway, "")
	}
	s.clientsLock.RUnlock()

	// close the listener
	_ = s.listener.Close()
}

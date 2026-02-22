package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	playlist4pb "github.com/devgianlu/go-librespot/proto/spotify/playlist4"
	"google.golang.org/protobuf/proto"
)

type fakeContextTrackPager struct {
	pages [][]*connectpb.ContextTrack
	errAt map[int]error
}

func (p fakeContextTrackPager) Page(_ context.Context, idx int) ([]*connectpb.ContextTrack, error) {
	if err, ok := p.errAt[idx]; ok {
		return nil, err
	}
	if idx >= len(p.pages) {
		return nil, io.EOF
	}
	return p.pages[idx], nil
}

func TestMetadataImageSize(t *testing.T) {
	if got := metadataImageSize(ApiRequestDataGetMetadata{ImageSize: "large"}, "default"); got != "large" {
		t.Fatalf("metadataImageSize should prefer request value, got %q", got)
	}
	if got := metadataImageSize(ApiRequestDataGetMetadata{}, "default"); got != "default" {
		t.Fatalf("metadataImageSize should fall back to default, got %q", got)
	}
}

func TestTrackNeedsEnrichment(t *testing.T) {
	if trackNeedsEnrichment(nil) {
		t.Fatal("nil track should not require enrichment")
	}

	if !trackNeedsEnrichment(&ApiResponseTrackFull{}) {
		t.Fatal("empty track should require enrichment")
	}

	okTrack := &ApiResponseTrackFull{
		Uri:      "spotify:track:4uLU6hMCjMI75M1A2tKUQC",
		Name:     "Song",
		Duration: 120000,
		Artists:  []ApiResponseRef{{Uri: "spotify:artist:4uLU6hMCjMI75M1A2tKUQC", Name: "Artist"}},
		Album:    &ApiResponseRef{Uri: "spotify:album:4uLU6hMCjMI75M1A2tKUQC", Name: "Album"},
	}
	if trackNeedsEnrichment(okTrack) {
		t.Fatal("fully populated track should not require enrichment")
	}

	noAlbumName := *okTrack
	noAlbumName.Album = &ApiResponseRef{Uri: okTrack.Album.Uri}
	if !trackNeedsEnrichment(&noAlbumName) {
		t.Fatal("track with empty album name should require enrichment")
	}
}

func TestRefNeedsNameEnrichment(t *testing.T) {
	if refNeedsNameEnrichment(nil) {
		t.Fatal("nil ref should not require enrichment")
	}
	if !refNeedsNameEnrichment(&ApiResponseRef{Uri: "spotify:album:4uLU6hMCjMI75M1A2tKUQC"}) {
		t.Fatal("ref with missing name should require enrichment")
	}
	if refNeedsNameEnrichment(&ApiResponseRef{Uri: "spotify:album:4uLU6hMCjMI75M1A2tKUQC", Name: "Album"}) {
		t.Fatal("ref with name should not require enrichment")
	}
}

func TestSpotifyIDFromBase62(t *testing.T) {
	id, err := spotifyIDFromBase62(librespot.SpotifyIdTypeTrack, "4uLU6hMCjMI75M1A2tKUQC")
	if err != nil {
		t.Fatalf("spotifyIDFromBase62 returned unexpected error: %v", err)
	}
	if id.Type() != librespot.SpotifyIdTypeTrack {
		t.Fatalf("spotifyIDFromBase62 type = %s, want %s", id.Type(), librespot.SpotifyIdTypeTrack)
	}

	_, err = spotifyIDFromBase62(librespot.SpotifyIdTypeTrack, "not-base62!")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("spotifyIDFromBase62 invalid input error = %v, want ErrBadRequest", err)
	}
}

func TestQueueTrackFromProvidedTrack(t *testing.T) {
	if got := queueTrackFromProvidedTrack(nil); got.Uri != "" || got.Name != "" || got.Provider != "" {
		t.Fatalf("queueTrackFromProvidedTrack(nil) = %+v, want zero value", got)
	}

	got := queueTrackFromProvidedTrack(&connectpb.ProvidedTrack{
		Uri:      "spotify:track:abc",
		Provider: "context",
		Metadata: map[string]string{"title": "Song Name"},
	})
	if got.Uri != "spotify:track:abc" || got.Provider != "context" || got.Name != "Song Name" {
		t.Fatalf("queueTrackFromProvidedTrack() unexpected value: %+v", got)
	}

	got = queueTrackFromProvidedTrack(&connectpb.ProvidedTrack{
		Uri:      "spotify:track:def",
		Provider: "queue",
	})
	if got.Name != "" {
		t.Fatalf("queueTrackFromProvidedTrack() should keep empty name when metadata is missing, got %+v", got)
	}
}

func TestQueueTracksFromProvidedTracks(t *testing.T) {
	if got := queueTracksFromProvidedTracks(nil); got == nil || len(got) != 0 {
		t.Fatalf("queueTracksFromProvidedTracks(nil) = %#v, want non-nil empty slice", got)
	}

	in := []*connectpb.ProvidedTrack{
		nil,
		{
			Uri:      "spotify:track:abc",
			Provider: "context",
			Metadata: map[string]string{"title": "Song"},
		},
	}
	got := queueTracksFromProvidedTracks(in)
	if len(got) != 1 {
		t.Fatalf("queueTracksFromProvidedTracks() len = %d, want 1", len(got))
	}
	if got[0].Uri != "spotify:track:abc" || got[0].Name != "Song" || got[0].Provider != "context" {
		t.Fatalf("queueTracksFromProvidedTracks() unexpected value: %+v", got[0])
	}
}

func TestEnrichQueueTrackNoSession(t *testing.T) {
	p := &AppPlayer{}
	in := ApiResponseQueueTrack{Uri: "spotify:track:4uLU6hMCjMI75M1A2tKUQC", Provider: "queue"}
	got := p.enrichQueueTrack(context.Background(), in, "default", newQueueEnrichmentCache())

	if got.Uri != in.Uri || got.Provider != in.Provider || got.Track != nil || got.Episode != nil {
		t.Fatalf("enrichQueueTrack should be a no-op without session, got %+v", got)
	}
}

func TestEnrichQueueTracksEmpty(t *testing.T) {
	p := &AppPlayer{}
	got := p.enrichQueueTracks(context.Background(), nil, "default", newQueueEnrichmentCache())
	if got == nil || len(got) != 0 {
		t.Fatalf("enrichQueueTracks(nil) = %#v, want non-nil empty slice", got)
	}
}

func TestRootlistMercuryCandidates(t *testing.T) {
	username := "alice"
	got := rootlistMercuryCandidates(username)
	if len(got) != 2 {
		t.Fatalf("rootlistMercuryCandidates should return two candidates, got %d", len(got))
	}

	expected := map[string]bool{
		"hm://playlist/user/alice/rootlist":    true,
		"hm://playlist/v2/user/alice/rootlist": true,
	}
	for _, uri := range got {
		if !expected[uri] {
			t.Fatalf("unexpected rootlist candidate %q", uri)
		}
		delete(expected, uri)
	}
	if len(expected) != 0 {
		t.Fatalf("missing rootlist candidates: %+v", expected)
	}
}

func TestPlaylistMercuryCandidates(t *testing.T) {
	got := playlistMercuryCandidates("3wV5G4zS3MiqBRVaRbgz05", "alice")
	expected := map[string]bool{
		"hm://playlist/v2/playlist/3wV5G4zS3MiqBRVaRbgz05":            true,
		"hm://playlist/playlist/3wV5G4zS3MiqBRVaRbgz05":               true,
		"hm://playlist/user/alice/playlist/3wV5G4zS3MiqBRVaRbgz05":    true,
		"hm://playlist/v2/user/alice/playlist/3wV5G4zS3MiqBRVaRbgz05": true,
	}
	if len(got) != len(expected) {
		t.Fatalf("playlistMercuryCandidates() len=%d, want %d", len(got), len(expected))
	}
	for _, uri := range got {
		if !expected[uri] {
			t.Fatalf("unexpected playlist mercury candidate %q", uri)
		}
		delete(expected, uri)
	}
	if len(expected) != 0 {
		t.Fatalf("missing playlist mercury candidates: %+v", expected)
	}

	if got := playlistMercuryCandidates("", "alice"); len(got) != 0 {
		t.Fatalf("playlistMercuryCandidates(empty id) = %+v, want empty", got)
	}
}

func TestContextResolveStatusCode(t *testing.T) {
	if code, ok := contextResolveStatusCode(nil); ok || code != 0 {
		t.Fatalf("contextResolveStatusCode(nil) = (%d, %t), want (0, false)", code, ok)
	}

	err := errors.New("invalid status code from context resolve: 404")
	if code, ok := contextResolveStatusCode(err); !ok || code != 404 {
		t.Fatalf("contextResolveStatusCode(404 err) = (%d, %t), want (404, true)", code, ok)
	}

	err = errors.New("some other error")
	if code, ok := contextResolveStatusCode(err); ok || code != 0 {
		t.Fatalf("contextResolveStatusCode(non-match err) = (%d, %t), want (0, false)", code, ok)
	}
}

func TestMercuryStatusCode(t *testing.T) {
	if code, ok := mercuryStatusCode(nil); ok || code != 0 {
		t.Fatalf("mercuryStatusCode(nil) = (%d, %t), want (0, false)", code, ok)
	}

	err := errors.New("mercury request failed with status code: 404")
	if code, ok := mercuryStatusCode(err); !ok || code != 404 {
		t.Fatalf("mercuryStatusCode(404 err) = (%d, %t), want (404, true)", code, ok)
	}

	err = errors.New("some other error")
	if code, ok := mercuryStatusCode(err); ok || code != 0 {
		t.Fatalf("mercuryStatusCode(non-match err) = (%d, %t), want (0, false)", code, ok)
	}
}

func TestNormalizeRootlistPlaylistURI(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{
			in:     "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05",
			want:   "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05",
			wantOK: true,
		},
		{
			in:     "spotify:user:1131355008:playlist:3wV5G4zS3MiqBRVaRbgz05",
			want:   "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05",
			wantOK: true,
		},
		{
			in:     "spotify:start-group:2e8ad7172fe1e808:playlists+van+vrienden",
			wantOK: false,
		},
		{
			in:     "spotify:end-group:2e8ad7172fe1e808",
			wantOK: false,
		},
		{
			in:     "spotify:user:1131355008:playlist:not_base62!",
			wantOK: false,
		},
		{
			in:     "spotify:playlist:not_base62!",
			wantOK: false,
		},
		{
			in:     "spotify:album:4uLU6hMCjMI75M1A2tKUQC",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		got, ok := normalizeRootlistPlaylistURI(tc.in)
		if ok != tc.wantOK {
			t.Fatalf("normalizeRootlistPlaylistURI(%q) ok=%t, want %t", tc.in, ok, tc.wantOK)
		}
		if got != tc.want {
			t.Fatalf("normalizeRootlistPlaylistURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRootlistFromSelectedListEmpty(t *testing.T) {
	if got := parseRootlistFromSelectedList(nil); got == nil || len(got) != 0 {
		t.Fatalf("parseRootlistFromSelectedList(nil) = %#v, want non-nil empty slice", got)
	}

	content := &playlist4pb.SelectedListContent{
		Contents: &playlist4pb.ListItems{
			Items: []*playlist4pb.Item{
				nil,
				{Uri: proto.String("")},
				{Uri: proto.String("spotify:user:1131355008:playlist:not_base62!")},
			},
		},
	}
	if got := parseRootlistFromSelectedList(content); got == nil || len(got) != 0 {
		t.Fatalf("parseRootlistFromSelectedList(invalid-only) = %#v, want non-nil empty slice", got)
	}
}

func TestParseRootlistFromSelectedList(t *testing.T) {
	name := "My Playlist"
	content := &playlist4pb.SelectedListContent{
		Contents: &playlist4pb.ListItems{
			Items: []*playlist4pb.Item{
				{Uri: proto.String("spotify:start-group:abc:folder")},
				{Uri: proto.String("spotify:user:1131355008:playlist:3wV5G4zS3MiqBRVaRbgz05")},
				{Uri: proto.String("spotify:playlist:3wV5G4zS3MiqBRVaRbgz05")}, // duplicate after normalize
				{Uri: proto.String("spotify:end-group:abc")},
			},
			MetaItems: []*playlist4pb.MetaItem{
				nil,
				{Attributes: &playlist4pb.ListAttributes{Name: &name}},
			},
		},
	}

	got := parseRootlistFromSelectedList(content)
	if len(got) != 1 {
		t.Fatalf("parseRootlistFromSelectedList() len=%d, want 1", len(got))
	}
	if got[0].Uri != "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05" {
		t.Fatalf("unexpected normalized uri: %q", got[0].Uri)
	}
	if got[0].Name != name {
		t.Fatalf("unexpected playlist name: %q", got[0].Name)
	}
}

func TestParseHelpers(t *testing.T) {
	if v, ok := parseBool("true"); !ok || !v {
		t.Fatalf("parseBool(true) = (%t, %t), want (true, true)", v, ok)
	}
	if v, ok := parseBool("0"); !ok || v {
		t.Fatalf("parseBool(0) = (%t, %t), want (false, true)", v, ok)
	}
	if _, ok := parseBool("nope"); ok {
		t.Fatal("parseBool(nope) should fail")
	}

	if v, ok := parseInt64(" 123 "); !ok || v != 123 {
		t.Fatalf("parseInt64(123) = (%d, %t), want (123, true)", v, ok)
	}
	if _, ok := parseInt64("abc"); ok {
		t.Fatal("parseInt64(abc) should fail")
	}

	if got := firstNonEmpty("", " ", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty() = %q, want x", got)
	}
	if got := firstNonEmptyFromMap(map[string]string{"a": " ", "b": "value"}, "a", "b"); got != "value" {
		t.Fatalf("firstNonEmptyFromMap() = %q, want value", got)
	}
	if got := firstNonEmptyFromMap(nil, "a"); got != "" {
		t.Fatalf("firstNonEmptyFromMap(nil) = %q, want empty", got)
	}
	if got := sanitizeContextDescription("12345"); got != "" {
		t.Fatalf("sanitizeContextDescription(numeric) = %q, want empty", got)
	}
	if got := sanitizeContextDescription(" Rock "); got != "Rock" {
		t.Fatalf("sanitizeContextDescription(text) = %q, want Rock", got)
	}

	if got := playlistOwnerFromContextURI("spotify:user:alice:playlist:3wV5G4zS3MiqBRVaRbgz05"); got != "alice" {
		t.Fatalf("playlistOwnerFromContextURI() = %q, want alice", got)
	}
	if got := playlistOwnerFromContextURI("spotify:playlist:3wV5G4zS3MiqBRVaRbgz05"); got != "" {
		t.Fatalf("playlistOwnerFromContextURI(spotify:playlist:...) = %q, want empty", got)
	}
	if got := playlistOwnerFromContextURI("spotify:user:alice"); got != "" {
		t.Fatalf("playlistOwnerFromContextURI(spotify:user:alice) = %q, want empty", got)
	}
}

func TestNormalizePlaylistImageURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "https://u.scdn.co/images/pl/default/ab67706c0000da8429b049a771662fae7b917d25",
			want: "/image/ab67706c0000da8429b049a771662fae7b917d25",
		},
		{
			in:   "spotify:image:AB67706C0000DA8429B049A771662FAE7B917D25",
			want: "/image/ab67706c0000da8429b049a771662fae7b917d25",
		},
		{
			in:   "AB67706C0000DA8429B049A771662FAE7B917D25",
			want: "/image/ab67706c0000da8429b049a771662fae7b917d25",
		},
		{
			in:   "https://i.scdn.co/image/ab67706c0000da8429b049a771662fae7b917d25",
			want: "/image/ab67706c0000da8429b049a771662fae7b917d25",
		},
		{
			in:   "spotify:image:nothex",
			want: "spotify:image:nothex",
		},
		{
			in:   "https://u.scdn.co/images/pl/default/nothex",
			want: "https://u.scdn.co/images/pl/default/nothex",
		},
		{
			in:   "",
			want: "",
		},
		{
			in:   "not-a-url",
			want: "not-a-url",
		},
	}

	for _, tc := range cases {
		if got := normalizePlaylistImageURL(tc.in); got != tc.want {
			t.Fatalf("normalizePlaylistImageURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlaylistMosaicHelpers(t *testing.T) {
	if got := playlistMosaicRenderSize(60); got != 60 {
		t.Fatalf("playlistMosaicRenderSize(60) = %d, want 60", got)
	}
	if got := playlistMosaicRenderSize(123); got != playlistMosaicDefaultSizePx {
		t.Fatalf("playlistMosaicRenderSize(invalid) = %d, want %d", got, playlistMosaicDefaultSizePx)
	}

	if got := playlistMosaicTrackImageSize(60); got != "small" {
		t.Fatalf("playlistMosaicTrackImageSize(60) = %q, want small", got)
	}
	if got := playlistMosaicTrackImageSize(640); got != "xlarge" {
		t.Fatalf("playlistMosaicTrackImageSize(640) = %q, want xlarge", got)
	}
	if got := playlistMosaicTrackImageSize(300); got != "default" {
		t.Fatalf("playlistMosaicTrackImageSize(300) = %q, want default", got)
	}

	if got := playlistMosaicOutputSizeFromImageSize("small"); got != 60 {
		t.Fatalf("playlistMosaicOutputSizeFromImageSize(small) = %d, want 60", got)
	}
	if got := playlistMosaicOutputSizeFromImageSize("xlarge"); got != 640 {
		t.Fatalf("playlistMosaicOutputSizeFromImageSize(xlarge) = %d, want 640", got)
	}
	if got := playlistMosaicOutputSizeFromImageSize("nope"); got != playlistMosaicDefaultSizePx {
		t.Fatalf("playlistMosaicOutputSizeFromImageSize(invalid) = %d, want %d", got, playlistMosaicDefaultSizePx)
	}

	if got := playlistGeneratedImagePath("3wV5G4zS3MiqBRVaRbgz05", 300); got != "/metadata/playlist/3wV5G4zS3MiqBRVaRbgz05/image?size=300" {
		t.Fatalf("playlistGeneratedImagePath() = %q", got)
	}
}

func TestIsHexImageID(t *testing.T) {
	if !isHexImageID("ab67706c0000da8429b049a771662fae7b917d25") {
		t.Fatal("expected valid hex image id")
	}
	if isHexImageID("ab67706c0000da8429b049a771662fae7b917d2") {
		t.Fatal("expected invalid hex image id length")
	}
	if isHexImageID("ab67706c0000da8429b049a771662fae7b917d2x") {
		t.Fatal("expected invalid hex image id characters")
	}
}

func TestPlaylistItemFromContextTrack(t *testing.T) {
	if _, ok := playlistItemFromContextTrack(nil); ok {
		t.Fatal("nil track should be skipped")
	}
	if _, ok := playlistItemFromContextTrack(&connectpb.ContextTrack{Uri: ""}); ok {
		t.Fatal("empty uri track should be skipped")
	}

	item, ok := playlistItemFromContextTrack(&connectpb.ContextTrack{
		Uri:      "spotify:track:1",
		Metadata: map[string]string{"added_by": "alice", "added_at": "123"},
	})
	if !ok {
		t.Fatal("valid track should be accepted")
	}
	if item.Uri != "spotify:track:1" || item.AddedBy != "alice" || item.AddedAt != 123 {
		t.Fatalf("unexpected playlist item: %+v", item)
	}
}

func TestPlaylistItemsFromPager(t *testing.T) {
	pager := fakeContextTrackPager{
		pages: [][]*connectpb.ContextTrack{
			{
				nil,
				{Uri: "spotify:track:1", Metadata: map[string]string{"added_by": "a"}},
			},
			{
				{Uri: "spotify:track:2", Metadata: map[string]string{"added_by": "b", "timestamp": "200"}},
				{Uri: "spotify:track:3"},
			},
		},
	}

	items, err := playlistItemsFromPager(context.Background(), pager, 1, 1)
	if err != nil {
		t.Fatalf("playlistItemsFromPager returned error: %v", err)
	}
	if len(items) != 1 || items[0].Uri != "spotify:track:2" || items[0].AddedAt != 200 {
		t.Fatalf("unexpected paged items: %+v", items)
	}

	items, err = playlistItemsFromPager(context.Background(), pager, 1, 0)
	if err != nil {
		t.Fatalf("playlistItemsFromPager returned error: %v", err)
	}
	if len(items) != 2 || items[1].Uri != "spotify:track:3" {
		t.Fatalf("unexpected full slice: %+v", items)
	}

	items, err = playlistItemsFromPager(context.Background(), pager, -5, 1)
	if err != nil {
		t.Fatalf("playlistItemsFromPager returned error: %v", err)
	}
	if len(items) != 1 || items[0].Uri != "spotify:track:1" {
		t.Fatalf("negative offset should behave like zero, got %+v", items)
	}
}

func TestPlaylistItemsFromPagerError(t *testing.T) {
	pager := fakeContextTrackPager{
		errAt: map[int]error{
			0: errors.New("boom"),
		},
	}
	if _, err := playlistItemsFromPager(context.Background(), pager, 0, 1); err == nil {
		t.Fatal("expected pager error")
	}
}

func TestPagePlaylistItems(t *testing.T) {
	items := []ApiResponsePlaylistItem{
		{Uri: "spotify:track:1"},
		{Uri: "spotify:track:2"},
		{Uri: "spotify:track:3"},
	}

	got := pagePlaylistItems(items, 1, 1)
	if len(got) != 1 || got[0].Uri != "spotify:track:2" {
		t.Fatalf("pagePlaylistItems offset=1 limit=1 unexpected: %+v", got)
	}

	got = pagePlaylistItems(items, -5, 0)
	if len(got) != 3 {
		t.Fatalf("pagePlaylistItems negative offset unexpected: %+v", got)
	}

	got = pagePlaylistItems(items, 99, 1)
	if got == nil || len(got) != 0 {
		t.Fatalf("pagePlaylistItems out of bounds expected non-nil empty slice, got %+v", got)
	}
}

func TestPageRootlistItems(t *testing.T) {
	items := []ApiResponseRootlistItem{
		{Uri: "spotify:playlist:1"},
		{Uri: "spotify:playlist:2"},
		{Uri: "spotify:playlist:3"},
	}

	got := pageRootlistItems(items, 1, 1)
	if len(got) != 1 || got[0].Uri != "spotify:playlist:2" {
		t.Fatalf("pageRootlistItems offset=1 limit=1 unexpected: %+v", got)
	}

	got = pageRootlistItems(items, -3, 0)
	if len(got) != 3 {
		t.Fatalf("pageRootlistItems negative offset unexpected: %+v", got)
	}

	got = pageRootlistItems(items, 99, 1)
	if got == nil || len(got) != 0 {
		t.Fatalf("pageRootlistItems out of bounds expected non-nil empty slice, got %+v", got)
	}
}

func TestCloneRootlistItems(t *testing.T) {
	in := []ApiResponseRootlistItem{
		{Uri: "spotify:playlist:1", Name: "One"},
		{Uri: "spotify:playlist:2", Name: "Two"},
	}

	out := cloneRootlistItems(in)
	if len(out) != len(in) {
		t.Fatalf("cloneRootlistItems len=%d, want %d", len(out), len(in))
	}
	if &out[0] == &in[0] {
		t.Fatal("cloneRootlistItems should copy backing array")
	}

	in[0].Name = "Changed"
	if out[0].Name != "One" {
		t.Fatalf("cloneRootlistItems should be isolated from source mutations, got %+v", out[0])
	}

	empty := cloneRootlistItems(nil)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("cloneRootlistItems(nil) = %#v, want non-nil empty slice", empty)
	}
}

func TestRootlistCache(t *testing.T) {
	p := &AppPlayer{}

	if _, ok := p.getRootlistCache(); ok {
		t.Fatal("expected empty cache miss")
	}

	p.putRootlistCache([]ApiResponseRootlistItem{
		{Uri: "spotify:playlist:1", Name: "One"},
	})

	got, ok := p.getRootlistCache()
	if !ok {
		t.Fatal("expected cache hit after put")
	}
	if len(got) != 1 || got[0].Uri != "spotify:playlist:1" || got[0].Name != "One" {
		t.Fatalf("unexpected cached items: %+v", got)
	}

	got[0].Name = "Mutated"
	gotAgain, ok := p.getRootlistCache()
	if !ok {
		t.Fatal("expected cache hit on second read")
	}
	if gotAgain[0].Name != "One" {
		t.Fatalf("cache should return isolated copies, got %+v", gotAgain[0])
	}

	p.rootlistCacheLock.Lock()
	p.rootlistCacheAt = time.Now().Add(-rootlistCacheTTL - time.Second)
	p.rootlistCacheLock.Unlock()

	if _, ok := p.getRootlistCache(); ok {
		t.Fatal("expected cache miss after ttl expiry")
	}
}

func TestUserDisplayNameFromContext(t *testing.T) {
	ctx := &connectpb.Context{
		Metadata: map[string]string{
			"display_name": "Henk",
		},
	}

	if got := userDisplayNameFromContext(ctx, "1131355008"); got != "Henk" {
		t.Fatalf("userDisplayNameFromContext() = %q, want Henk", got)
	}
	if got := userDisplayNameFromContext(ctx, "Henk"); got != "" {
		t.Fatalf("userDisplayNameFromContext() should ignore username echo, got %q", got)
	}
	if got := userDisplayNameFromContext(nil, "1131355008"); got != "" {
		t.Fatalf("userDisplayNameFromContext(nil) = %q, want empty", got)
	}
}

func TestUserDisplayNameFromJSON(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		fallback string
		want     string
	}{
		{
			name:     "display_name top-level",
			payload:  `{"display_name":"Henk"}`,
			fallback: "1131355008",
			want:     "Henk",
		},
		{
			name:     "camel displayName top-level",
			payload:  `{"displayName":"Henk"}`,
			fallback: "1131355008",
			want:     "Henk",
		},
		{
			name:     "name nested profile",
			payload:  `{"profile":{"name":"Henk"}}`,
			fallback: "1131355008",
			want:     "Henk",
		},
		{
			name:     "name nested user",
			payload:  `{"user":{"display_name":"Henk"}}`,
			fallback: "1131355008",
			want:     "Henk",
		},
		{
			name:     "username echo filtered",
			payload:  `{"name":"1131355008"}`,
			fallback: "1131355008",
			want:     "",
		},
		{
			name:     "invalid json",
			payload:  `{`,
			fallback: "1131355008",
			want:     "",
		},
	}

	for _, tc := range cases {
		if got := userDisplayNameFromJSON([]byte(tc.payload), tc.fallback); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestSanitizeResolvedDisplayName(t *testing.T) {
	if got := sanitizeResolvedDisplayName("1131355008", "1131355008"); got != "" {
		t.Fatalf("sanitizeResolvedDisplayName should drop username echo, got %q", got)
	}
	if got := sanitizeResolvedDisplayName("  Henk  ", "1131355008"); got != "Henk" {
		t.Fatalf("sanitizeResolvedDisplayName should trim and keep display name, got %q", got)
	}
}

func TestOwnerDisplayNameCache(t *testing.T) {
	p := &AppPlayer{}
	if _, ok := p.getOwnerDisplayNameCache("alice"); ok {
		t.Fatal("expected empty cache miss")
	}

	p.putOwnerDisplayNameCache("alice", "Alice")
	if got, ok := p.getOwnerDisplayNameCache("alice"); !ok || got != "Alice" {
		t.Fatalf("unexpected cache hit state: value=%q ok=%t", got, ok)
	}

	p.ownerDisplayNameCacheLock.Lock()
	p.ownerDisplayNameCache["alice"] = displayNameCacheEntry{
		Value:    "Alice",
		CachedAt: time.Now().Add(-ownerDisplayNameCacheTTL - time.Second),
	}
	p.ownerDisplayNameCacheLock.Unlock()
	if _, ok := p.getOwnerDisplayNameCache("alice"); ok {
		t.Fatal("expected owner display cache miss after ttl expiry")
	}
}

func TestParsePlaylistFromContext(t *testing.T) {
	ctx := &connectpb.Context{
		Uri: "spotify:user:alice:playlist:3wV5G4zS3MiqBRVaRbgz05",
		Metadata: map[string]string{
			"title":                     "Playlist Name",
			"context_description":       "Desc",
			"owner_display_name":        "Alice Cooper",
			"collaborative":             "true",
			"image_url":                 "https://u.scdn.co/images/pl/default/ab67706c0000da8429b049a771662fae7b917d25",
			"playlist_number_of_tracks": "3",
		},
		Pages: []*connectpb.ContextPage{
			nil,
			{
				Metadata: map[string]string{"name": "Ignored page name"},
				Tracks: []*connectpb.ContextTrack{
					nil,
					{Uri: "spotify:track:1", Metadata: map[string]string{"added_by": "alice", "added_at": "100"}},
					{Uri: "spotify:track:2", Metadata: map[string]string{"added_by": "bob", "timestamp": "200"}},
					{Uri: "spotify:track:3", Metadata: map[string]string{}},
				},
			},
		},
	}

	resp := parsePlaylistFromContext(ctx, "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05", ApiRequestDataGetMetadata{Limit: 2, Offset: 1})
	if resp.Uri != ctx.Uri {
		t.Fatalf("playlist uri = %q, want %q", resp.Uri, ctx.Uri)
	}
	if resp.Name != "Playlist Name" || resp.Description != "Desc" {
		t.Fatalf("playlist metadata mismatch: %+v", resp)
	}
	if resp.OwnerUsername != "alice" {
		t.Fatalf("owner username = %q, want alice", resp.OwnerUsername)
	}
	if resp.OwnerDisplayName != "Alice Cooper" {
		t.Fatalf("owner display name = %q, want Alice Cooper", resp.OwnerDisplayName)
	}
	if !resp.Collaborative {
		t.Fatal("collaborative should be true")
	}
	if resp.ImageUrl == nil || *resp.ImageUrl != "/image/ab67706c0000da8429b049a771662fae7b917d25" {
		t.Fatalf("image url mismatch: %+v", resp.ImageUrl)
	}
	if resp.TotalTracks != 3 {
		t.Fatalf("total tracks = %d, want 3", resp.TotalTracks)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("paged items len = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Uri != "spotify:track:2" || resp.Items[0].AddedBy != "bob" || resp.Items[0].AddedAt != 200 {
		t.Fatalf("unexpected first paged item: %+v", resp.Items[0])
	}
	if resp.Items[1].Uri != "spotify:track:3" {
		t.Fatalf("unexpected second paged item: %+v", resp.Items[1])
	}

	resp = parsePlaylistFromContext(ctx, "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05", ApiRequestDataGetMetadata{Limit: 1, Offset: 1})
	if len(resp.Items) != 1 || resp.Items[0].Uri != "spotify:track:2" {
		t.Fatalf("expected strict limit paging, got %+v", resp.Items)
	}
}

func TestParsePlaylistFromContextNil(t *testing.T) {
	resp := parsePlaylistFromContext(nil, "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05", ApiRequestDataGetMetadata{})
	if resp.Uri != "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05" || len(resp.Items) != 0 {
		t.Fatalf("parsePlaylistFromContext(nil) unexpected response: %+v", resp)
	}
}

func TestParsePlaylistFromContextFallbacks(t *testing.T) {
	ctx := &connectpb.Context{
		Metadata: map[string]string{
			"name":             "Fallback Playlist",
			"is_collaborative": "false",
			"picture":          "spotify:image:AB67706C0000DA8429B049A771662FAE7B917D25",
			"total_tracks":     "not-a-number",
		},
		Pages: []*connectpb.ContextPage{
			{
				Tracks: []*connectpb.ContextTrack{
					{Uri: ""},
					{Uri: "spotify:track:1", Metadata: map[string]string{"timestamp": "bad"}},
				},
			},
		},
	}

	fallbackURI := "spotify:user:bob:playlist:3wV5G4zS3MiqBRVaRbgz05"
	resp := parsePlaylistFromContext(ctx, fallbackURI, ApiRequestDataGetMetadata{Offset: -5})
	if resp.Uri != fallbackURI {
		t.Fatalf("fallback uri mismatch: %q", resp.Uri)
	}
	if resp.OwnerUsername != "bob" {
		t.Fatalf("owner fallback mismatch: %q", resp.OwnerUsername)
	}
	if resp.Collaborative {
		t.Fatal("collaborative should be false")
	}
	if resp.ImageUrl == nil || *resp.ImageUrl != "/image/ab67706c0000da8429b049a771662fae7b917d25" {
		t.Fatalf("image fallback mismatch: %+v", resp.ImageUrl)
	}
	if resp.TotalTracks != 1 {
		t.Fatalf("total tracks fallback mismatch: %d", resp.TotalTracks)
	}
	if len(resp.Items) != 1 || resp.Items[0].Uri != "spotify:track:1" || resp.Items[0].AddedAt != 0 {
		t.Fatalf("unexpected items: %+v", resp.Items)
	}

	resp = parsePlaylistFromContext(ctx, fallbackURI, ApiRequestDataGetMetadata{Offset: 5})
	if len(resp.Items) != 0 {
		t.Fatalf("offset beyond items should return empty page, got %+v", resp.Items)
	}
}

func TestApplyPlaylistSelectedListMetadata(t *testing.T) {
	resp := &ApiResponsePlaylistFull{
		Uri:           "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05",
		Name:          "",
		Description:   "",
		OwnerUsername: "",
		TotalTracks:   0,
	}
	content := &playlist4pb.SelectedListContent{
		Length:        proto.Int32(42),
		OwnerUsername: proto.String("alice"),
		Attributes: &playlist4pb.ListAttributes{
			Name:          proto.String("Playlist Name"),
			Description:   proto.String("Playlist Description"),
			Collaborative: proto.Bool(true),
			PictureSize: []*playlist4pb.PictureSize{
				{Url: proto.String("https://u.scdn.co/images/pl/default/ab67706c0000da8429b049a771662fae7b917d25")},
			},
		},
	}

	p := &AppPlayer{}
	p.applyPlaylistSelectedListMetadata(resp, content)

	if resp.Name != "Playlist Name" || resp.Description != "Playlist Description" {
		t.Fatalf("playlist metadata not applied: %+v", resp)
	}
	if resp.OwnerUsername != "alice" {
		t.Fatalf("owner username not applied: %q", resp.OwnerUsername)
	}
	if !resp.Collaborative {
		t.Fatal("collaborative flag not applied")
	}
	if resp.TotalTracks != 42 {
		t.Fatalf("total tracks not applied: %d", resp.TotalTracks)
	}
	if resp.ImageUrl == nil || *resp.ImageUrl != "/image/ab67706c0000da8429b049a771662fae7b917d25" {
		t.Fatalf("image url not applied correctly: %+v", resp.ImageUrl)
	}
}

func TestApplyPlaylistSelectedListMetadataMetaItemsFallback(t *testing.T) {
	resp := &ApiResponsePlaylistFull{
		Uri: "spotify:playlist:3wV5G4zS3MiqBRVaRbgz05",
	}
	content := &playlist4pb.SelectedListContent{
		Contents: &playlist4pb.ListItems{
			MetaItems: []*playlist4pb.MetaItem{
				{
					OwnerUsername: proto.String("bob"),
					Length:        proto.Int32(7),
					Attributes: &playlist4pb.ListAttributes{
						Name:        proto.String("Meta Name"),
						Description: proto.String("Meta Description"),
						PictureSize: []*playlist4pb.PictureSize{
							{Url: proto.String("spotify:image:AB67706C0000DA8429B049A771662FAE7B917D25")},
						},
					},
				},
			},
		},
	}

	p := &AppPlayer{}
	p.applyPlaylistSelectedListMetadata(resp, content)

	if resp.Name != "Meta Name" || resp.Description != "Meta Description" {
		t.Fatalf("meta fallback attributes not applied: %+v", resp)
	}
	if resp.OwnerUsername != "bob" {
		t.Fatalf("meta fallback owner not applied: %q", resp.OwnerUsername)
	}
	if resp.TotalTracks != 7 {
		t.Fatalf("meta fallback length not applied: %d", resp.TotalTracks)
	}
	if resp.ImageUrl == nil || *resp.ImageUrl != "/image/ab67706c0000da8429b049a771662fae7b917d25" {
		t.Fatalf("meta fallback image not applied: %+v", resp.ImageUrl)
	}
}

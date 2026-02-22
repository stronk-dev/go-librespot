package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

func TestParseIntQuery(t *testing.T) {
	query := url.Values{}

	val, ok := parseIntQuery(query, "limit", 1)
	if !ok || val != 0 {
		t.Fatalf("empty query should return (0, true), got (%d, %t)", val, ok)
	}

	query.Set("limit", "10")
	val, ok = parseIntQuery(query, "limit", 1)
	if !ok || val != 10 {
		t.Fatalf("valid int should return (10, true), got (%d, %t)", val, ok)
	}

	query.Set("limit", "abc")
	if _, ok := parseIntQuery(query, "limit", 1); ok {
		t.Fatal("non-numeric value should fail")
	}

	query.Set("limit", "0")
	if _, ok := parseIntQuery(query, "limit", 1); ok {
		t.Fatal("value below minimum should fail")
	}
}

func TestIsSpotifyBase62ID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{id: "4uLU6hMCjMI75M1A2tKUQ", want: true},    // 21
		{id: "4uLU6hMCjMI75M1A2tKUQC", want: true},   // 22
		{id: "4uLU6hMCjMI75M1A2tKU", want: false},    // 20
		{id: "4uLU6hMCjMI75M1A2tKUQCC", want: false}, // 23
		{id: "4uLU6hMCjMI75M1A2tKUQ-", want: false},
		{id: "4uLU6hMCjMI75M1A2tKUQ_", want: false},
	}

	for _, tc := range cases {
		if got := isSpotifyBase62ID(tc.id); got != tc.want {
			t.Fatalf("isSpotifyBase62ID(%q) = %t, want %t", tc.id, got, tc.want)
		}
	}
}

func TestParseMetadataRequest(t *testing.T) {
	id := "4uLU6hMCjMI75M1A2tKUQC"

	req := httptest.NewRequest(http.MethodGet, "/metadata/track/"+id+"?image_size=small", nil)
	data, ok := parseMetadataRequest(req, "/metadata/track/", false)
	if !ok {
		t.Fatal("expected metadata request parse to succeed")
	}
	if data.Id != id || data.ImageSize != "small" || data.Limit != 0 || data.Offset != 0 {
		t.Fatalf("unexpected parse result: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"?limit=20&offset=5", nil)
	data, ok = parseMetadataRequest(req, "/metadata/playlist/", true)
	if !ok {
		t.Fatal("expected metadata request parse with paging to succeed")
	}
	if data.Id != id || data.Limit != 20 || data.Offset != 5 {
		t.Fatalf("unexpected parse result with paging: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/track/not_base62!", nil)
	if _, ok := parseMetadataRequest(req, "/metadata/track/", false); ok {
		t.Fatal("expected invalid ID parse to fail")
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"?limit=0", nil)
	if _, ok := parseMetadataRequest(req, "/metadata/playlist/", true); ok {
		t.Fatal("expected invalid limit parse to fail")
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"?offset=-1", nil)
	if _, ok := parseMetadataRequest(req, "/metadata/playlist/", true); ok {
		t.Fatal("expected invalid offset parse to fail")
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"?limit=nope", nil)
	if _, ok := parseMetadataRequest(req, "/metadata/playlist/", true); ok {
		t.Fatal("expected invalid non-numeric limit parse to fail")
	}
}

func TestParseRootlistRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metadata/rootlist", nil)
	data, ok := parseRootlistRequest(req)
	if !ok {
		t.Fatal("expected rootlist request parse to succeed")
	}
	if data.Paginate || data.Limit != 0 || data.Offset != 0 {
		t.Fatalf("unexpected parse result without paging params: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/rootlist?limit=20&offset=5", nil)
	data, ok = parseRootlistRequest(req)
	if !ok {
		t.Fatal("expected rootlist request parse with paging to succeed")
	}
	if !data.Paginate || data.Limit != 20 || data.Offset != 5 {
		t.Fatalf("unexpected parse result with paging params: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/rootlist?offset=5", nil)
	data, ok = parseRootlistRequest(req)
	if !ok {
		t.Fatal("expected rootlist request parse with offset-only to succeed")
	}
	if !data.Paginate || data.Limit != rootlistPageLimitDefault || data.Offset != 5 {
		t.Fatalf("unexpected parse result with offset-only: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/rootlist?limit=0", nil)
	if _, ok := parseRootlistRequest(req); ok {
		t.Fatal("expected invalid limit parse to fail")
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/rootlist?offset=-1", nil)
	if _, ok := parseRootlistRequest(req); ok {
		t.Fatal("expected invalid offset parse to fail")
	}
}

func TestParsePlaylistImageRequest(t *testing.T) {
	id := "4uLU6hMCjMI75M1A2tKUQC"

	req := httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"/image", nil)
	data, ok := parsePlaylistImageRequest(req, "/metadata/playlist/")
	if !ok {
		t.Fatal("expected playlist image request parse to succeed")
	}
	if data.Id != id || data.Size != playlistImageSizeDefault || data.IfNoneMatch != "" {
		t.Fatalf("unexpected default parse result: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"/image?size=640", nil)
	req.Header.Set("If-None-Match", "\"abc\"")
	data, ok = parsePlaylistImageRequest(req, "/metadata/playlist/")
	if !ok {
		t.Fatal("expected playlist image request parse with size to succeed")
	}
	if data.Id != id || data.Size != 640 || data.IfNoneMatch != "\"abc\"" {
		t.Fatalf("unexpected parse result with query/header: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id, nil)
	if _, ok := parsePlaylistImageRequest(req, "/metadata/playlist/"); ok {
		t.Fatal("expected non-image playlist path parse to fail")
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/not_base62!/image", nil)
	if _, ok := parsePlaylistImageRequest(req, "/metadata/playlist/"); ok {
		t.Fatal("expected invalid id parse to fail")
	}

	req = httptest.NewRequest(http.MethodGet, "/metadata/playlist/"+id+"/image?size=123", nil)
	if _, ok := parsePlaylistImageRequest(req, "/metadata/playlist/"); ok {
		t.Fatal("expected unsupported size parse to fail")
	}
}

func TestParseImageProxyRequest(t *testing.T) {
	id := "ab67706c0000da8429b049a771662fae7b917d25"

	req := httptest.NewRequest(http.MethodGet, "/image/"+id, nil)
	req.Header.Set("If-None-Match", "\"abc\"")
	data, ok := parseImageProxyRequest(req, "/image/")
	if !ok {
		t.Fatal("expected image proxy request parse to succeed")
	}
	if data.Id != id || data.IfNoneMatch != "\"abc\"" {
		t.Fatalf("unexpected parse result: %+v", data)
	}

	req = httptest.NewRequest(http.MethodGet, "/image/nothex", nil)
	if _, ok := parseImageProxyRequest(req, "/image/"); ok {
		t.Fatal("expected invalid id parse to fail")
	}
}

func TestSpotifyURIFromGID(t *testing.T) {
	gid := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	want := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, gid).Uri()
	got := spotifyURIFromGID(librespot.SpotifyIdTypeTrack, gid)
	if got != want {
		t.Fatalf("spotifyURIFromGID() = %q, want %q", got, want)
	}

	if got := spotifyURIFromGID(librespot.SpotifyIdTypeTrack, gid[:15]); got != "" {
		t.Fatalf("spotifyURIFromGID() with invalid gid length = %q, want empty", got)
	}
}

func TestArtistRefAndAlbumRef(t *testing.T) {
	if got := artistRef(nil); got.Uri != "" || got.Name != "" {
		t.Fatalf("artistRef(nil) = %+v, want zero value", got)
	}
	if got := albumRef(nil); got.Uri != "" || got.Name != "" {
		t.Fatalf("albumRef(nil) = %+v, want zero value", got)
	}

	artistName := "Artist"
	albumName := "Album"
	gid := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	artist := &metadatapb.Artist{Gid: gid, Name: &artistName}
	album := &metadatapb.Album{Gid: gid, Name: &albumName}

	artistGot := artistRef(artist)
	albumGot := albumRef(album)

	if artistGot.Name != artistName || artistGot.Uri == "" {
		t.Fatalf("artistRef() unexpected value: %+v", artistGot)
	}
	if albumGot.Name != albumName || albumGot.Uri == "" {
		t.Fatalf("albumRef() unexpected value: %+v", albumGot)
	}

	artistBad := &metadatapb.Artist{Name: &artistName, Gid: []byte{1, 2, 3}}
	albumBad := &metadatapb.Album{Name: &albumName, Gid: []byte{1, 2, 3}}
	if got := artistRef(artistBad); got.Uri != "" || got.Name != artistName {
		t.Fatalf("artistRef() with invalid gid unexpected value: %+v", got)
	}
	if got := albumRef(albumBad); got.Uri != "" || got.Name != albumName {
		t.Fatalf("albumRef() with invalid gid unexpected value: %+v", got)
	}
}

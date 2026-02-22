package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

type displayNameCacheEntry struct {
	Value    string
	CachedAt time.Time
}

const (
	ownerDisplayNameCacheTTL         = 30 * time.Minute
	ownerDisplayNameNegativeCacheTTL = 2 * time.Minute
	ownerDisplayNameCacheMaxEntries  = 1024
)

func userDisplayNameFromContext(spotCtx *connectpb.Context, fallbackUsername string) string {
	if spotCtx == nil {
		return ""
	}

	ctxMeta := spotCtx.Metadata
	pageMeta := map[string]string{}
	for _, page := range spotCtx.Pages {
		if page != nil && len(page.Metadata) > 0 {
			pageMeta = page.Metadata
			break
		}
	}

	name := firstNonEmpty(
		firstNonEmptyFromMap(ctxMeta, "display_name", "owner_display_name", "owner_name", "name", "title", "username"),
		firstNonEmptyFromMap(pageMeta, "display_name", "owner_display_name", "owner_name", "name", "title", "username"),
	)
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if fallbackUsername != "" && strings.EqualFold(name, fallbackUsername) {
		return ""
	}

	return name
}

func sanitizeResolvedDisplayName(name, fallbackUsername string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if fallbackUsername != "" && strings.EqualFold(name, fallbackUsername) {
		return ""
	}
	return name
}

type userDisplayNamePayload struct {
	DisplayName  string `json:"display_name"`
	DisplayName2 string `json:"displayName"`
	Name         string `json:"name"`
	Profile      *struct {
		DisplayName  string `json:"display_name"`
		DisplayName2 string `json:"displayName"`
		Name         string `json:"name"`
	} `json:"profile"`
	User *struct {
		DisplayName  string `json:"display_name"`
		DisplayName2 string `json:"displayName"`
		Name         string `json:"name"`
	} `json:"user"`
}

func userDisplayNameFromJSON(payload []byte, fallbackUsername string) string {
	if len(payload) == 0 {
		return ""
	}

	var data userDisplayNamePayload
	if err := json.Unmarshal(payload, &data); err != nil {
		return ""
	}

	name := sanitizeResolvedDisplayName(firstNonEmpty(data.DisplayName, data.DisplayName2, data.Name), fallbackUsername)
	if name != "" {
		return name
	}

	if data.Profile != nil {
		name = sanitizeResolvedDisplayName(firstNonEmpty(data.Profile.DisplayName, data.Profile.DisplayName2, data.Profile.Name), fallbackUsername)
		if name != "" {
			return name
		}
	}
	if data.User != nil {
		name = sanitizeResolvedDisplayName(firstNonEmpty(data.User.DisplayName, data.User.DisplayName2, data.User.Name), fallbackUsername)
		if name != "" {
			return name
		}
	}

	return ""
}

func (p *AppPlayer) getOwnerDisplayNameCache(username string) (string, bool) {
	p.ownerDisplayNameCacheLock.RLock()
	defer p.ownerDisplayNameCacheLock.RUnlock()

	entry, ok := p.ownerDisplayNameCache[username]
	if !ok {
		return "", false
	}

	ttl := ownerDisplayNameCacheTTL
	if entry.Value == "" {
		ttl = ownerDisplayNameNegativeCacheTTL
	}
	if time.Since(entry.CachedAt) > ttl {
		return "", false
	}

	return entry.Value, true
}

func (p *AppPlayer) putOwnerDisplayNameCache(username, name string) {
	p.ownerDisplayNameCacheLock.Lock()
	defer p.ownerDisplayNameCacheLock.Unlock()

	if p.ownerDisplayNameCache == nil {
		p.ownerDisplayNameCache = make(map[string]displayNameCacheEntry)
	}
	if _, exists := p.ownerDisplayNameCache[username]; !exists {
		p.ownerDisplayNameCacheOrder = append(p.ownerDisplayNameCacheOrder, username)
	}
	p.ownerDisplayNameCache[username] = displayNameCacheEntry{Value: name, CachedAt: time.Now()}

	for len(p.ownerDisplayNameCacheOrder) > ownerDisplayNameCacheMaxEntries {
		evict := p.ownerDisplayNameCacheOrder[0]
		p.ownerDisplayNameCacheOrder = p.ownerDisplayNameCacheOrder[1:]
		delete(p.ownerDisplayNameCache, evict)
	}
}

func (p *AppPlayer) resolveOwnerDisplayNameViaContext(ctx context.Context, username string) string {
	spotCtx, err := p.sess.Spclient().ContextResolve(ctx, "spotify:user:"+username)
	if err != nil {
		p.app.log.WithError(err).WithField("owner_username", username).Debug("failed resolving owner display name context")
		return ""
	}
	return userDisplayNameFromContext(spotCtx, username)
}

func (p *AppPlayer) resolveOwnerDisplayNameViaUserProfileView(ctx context.Context, username string) string {
	resp, err := p.sess.Spclient().Request(
		ctx,
		http.MethodGet,
		fmt.Sprintf("/user-profile-view/v3/profile/%s", url.PathEscape(username)),
		url.Values{
			"playlist_limit": []string{"0"},
			"artist_limit":   []string{"0"},
			"episode_limit":  []string{"0"},
		},
		nil,
		nil,
	)
	if err != nil {
		p.app.log.WithError(err).WithField("owner_username", username).Debug("failed resolving owner display name via user-profile-view")
		return ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		p.app.log.WithField("owner_username", username).WithField("status_code", resp.StatusCode).
			Debug("user-profile-view request for owner display name failed")
		return ""
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.app.log.WithError(err).WithField("owner_username", username).Debug("failed reading user-profile-view response")
		return ""
	}

	return userDisplayNameFromJSON(body, username)
}

func (p *AppPlayer) resolveOwnerDisplayName(ctx context.Context, username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}

	if name, ok := p.getOwnerDisplayNameCache(username); ok {
		return name
	}

	if name := p.resolveOwnerDisplayNameViaContext(ctx, username); name != "" {
		p.putOwnerDisplayNameCache(username, name)
		return name
	}
	if name := p.resolveOwnerDisplayNameViaUserProfileView(ctx, username); name != "" {
		p.putOwnerDisplayNameCache(username, name)
		return name
	}

	p.putOwnerDisplayNameCache(username, "")
	return ""
}

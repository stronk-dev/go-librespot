package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	librespot "github.com/devgianlu/go-librespot"
	"golang.org/x/oauth2"
	spotifyoauth2 "golang.org/x/oauth2/spotify"
)

var defaultDevApiScopes = []string{
	"user-read-playback-state",
	"user-read-currently-playing",
	"user-library-read",
	"user-library-modify",
	"playlist-read-private",
	"playlist-read-collaborative",
	"playlist-modify-public",
	"playlist-modify-private",
	"user-read-recently-played",
	"user-top-read",
	"user-follow-read",
	"user-read-private",
}

// DevApiConfig holds the configuration for the Spotify Developer API integration.
type DevApiConfig struct {
	ClientId     string   `koanf:"client_id"`
	ClientSecret string   `koanf:"client_secret"`
	RedirectUri  string   `koanf:"redirect_uri"`
	Scopes       []string `koanf:"scopes"`
}

// DevApiTokenProvider manages a separate Spotify Developer API OAuth2 token
// for /web-api/ requests, avoiding rate limit conflicts with the internal token.
type DevApiTokenProvider struct {
	log    librespot.Logger
	cfg    DevApiConfig
	state  *librespot.AppState
	client *http.Client

	oauthConf *oauth2.Config

	mu       sync.RWMutex
	verifier string       // PKCE verifier for in-progress authorization
	token    *oauth2.Token // current token (access + refresh)
}

// NewDevApiTokenProvider creates a new provider. It restores any persisted
// refresh token from state and sets up the OAuth2 configuration.
func NewDevApiTokenProvider(log librespot.Logger, cfg DevApiConfig, state *librespot.AppState, client *http.Client) *DevApiTokenProvider {
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultDevApiScopes
	}

	p := &DevApiTokenProvider{
		log:    log,
		cfg:    cfg,
		state:  state,
		client: client,
		oauthConf: &oauth2.Config{
			ClientID:     cfg.ClientId,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectUri,
			Scopes:       scopes,
			Endpoint:     spotifyoauth2.Endpoint,
		},
	}

	// Restore persisted token if available.
	if state.DevApiToken != nil && state.DevApiToken.RefreshToken != "" {
		p.token = &oauth2.Token{
			AccessToken:  state.DevApiToken.AccessToken,
			RefreshToken: state.DevApiToken.RefreshToken,
			Expiry:       state.DevApiToken.Expiry,
		}
		log.Infof("dev API: restored token from state")
	}

	return p
}

// IsAuthorized returns true if we have a token (valid or refreshable).
func (p *DevApiTokenProvider) IsAuthorized() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.token != nil
}

// AuthorizeURL generates the OAuth2 authorization URL for the user to visit.
func (p *DevApiTokenProvider) AuthorizeURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.verifier = oauth2.GenerateVerifier()
	return p.oauthConf.AuthCodeURL("devapi", oauth2.S256ChallengeOption(p.verifier))
}

// HandleCallback exchanges the authorization code for tokens and persists them.
func (p *DevApiTokenProvider) HandleCallback(ctx context.Context, code string) error {
	p.mu.Lock()
	verifier := p.verifier
	p.mu.Unlock()

	if verifier == "" {
		return fmt.Errorf("no pending authorization (visit /devapi/authorize first)")
	}

	token, err := p.oauthConf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("failed exchanging code: %w", err)
	}

	p.mu.Lock()
	p.token = token
	p.verifier = ""
	p.mu.Unlock()

	if err := p.persistToken(); err != nil {
		p.log.WithError(err).Error("dev API: failed persisting token")
	}

	p.log.Infof("dev API: authorization successful")
	return nil
}

// WebApiRequest makes an authenticated request to api.spotify.com using the
// dev API token. Automatically refreshes if expired.
func (p *DevApiTokenProvider) WebApiRequest(ctx context.Context, method, path string, query url.Values) (*http.Response, error) {
	p.mu.RLock()
	token := p.token
	p.mu.RUnlock()

	if token == nil {
		return nil, fmt.Errorf("dev API not authorized")
	}

	// Use oauth2 token source for automatic refresh.
	ts := p.oauthConf.TokenSource(ctx, token)
	newToken, err := ts.Token()
	if err != nil {
		p.log.WithError(err).Error("dev API: token refresh failed")
		p.mu.Lock()
		p.token = nil
		p.mu.Unlock()
		return nil, fmt.Errorf("dev API token refresh failed: %w", err)
	}

	// Persist if token was refreshed.
	if newToken.AccessToken != token.AccessToken {
		p.mu.Lock()
		p.token = newToken
		p.mu.Unlock()

		if err := p.persistToken(); err != nil {
			p.log.WithError(err).Error("dev API: failed persisting refreshed token")
		}
	}

	// Build request to api.spotify.com.
	reqURL, _ := url.Parse("https://api.spotify.com/")
	reqURL = reqURL.JoinPath(path)
	if query != nil {
		reqURL.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed creating request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", newToken.AccessToken))

	return p.client.Do(req)
}

func (p *DevApiTokenProvider) persistToken() error {
	p.mu.RLock()
	token := p.token
	p.mu.RUnlock()

	if token == nil {
		return nil
	}

	p.state.DevApiToken = &librespot.DevApiTokenState{
		RefreshToken: token.RefreshToken,
		AccessToken:  token.AccessToken,
		Expiry:       token.Expiry,
	}
	return p.state.Write()
}

// devApiDefaultRedirectUri derives a default redirect URI from the server config.
func devApiDefaultRedirectUri(address string, port int) string {
	host := address
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d/devapi/callback", host, port)
}

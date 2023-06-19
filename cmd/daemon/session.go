package main

import (
	"encoding/xml"
	"fmt"
	log "github.com/sirupsen/logrus"
	librespot "go-librespot"
	"go-librespot/ap"
	"go-librespot/dealer"
	"go-librespot/login5"
	connectpb "go-librespot/proto/spotify/connectstate/model"
	credentialspb "go-librespot/proto/spotify/login5/v3/credentials"
	"go-librespot/spclient"
	"strings"
	"time"
)

const VolumeSteps = 64

type Session struct {
	app *App

	stop chan struct{}

	ap     *ap.Accesspoint
	login5 *login5.Login5
	sp     *spclient.Spclient
	dealer *dealer.Dealer

	spotConnId string
}

func (s *Session) handleAccesspointPacket(pktType ap.PacketType, payload []byte) error {
	switch pktType {
	case ap.PacketTypeProductInfo:
		var prod ProductInfo
		if err := xml.Unmarshal(payload, &prod); err != nil {
			return fmt.Errorf("failed umarshalling ProductInfo: %w", err)
		}

		// TODO: we may need this
		return nil
	default:
		return nil
	}
}

func (s *Session) handleDealerMessage(msg dealer.Message) error {
	if strings.HasPrefix(msg.Uri, "hm://pusher/v1/connections/") {
		s.spotConnId = msg.Headers["Spotify-Connection-Id"]
		log.Debugf("received connection id: %s", s.spotConnId)

		// put the initial state
		if err := s.sp.PutConnectState(s.spotConnId, &connectpb.PutStateRequest{
			ClientSideTimestamp: uint64(time.Now().UnixMilli()),
			MemberType:          connectpb.MemberType_CONNECT_STATE,
			PutStateReason:      connectpb.PutStateReason_NEW_DEVICE,
			Device: &connectpb.Device{
				DeviceInfo: &connectpb.DeviceInfo{
					CanPlay:               true,
					Volume:                0, // TODO
					Name:                  s.app.deviceName,
					DeviceId:              s.app.deviceId,
					DeviceType:            s.app.deviceType,
					DeviceSoftwareVersion: librespot.VersionString(),
					ClientId:              librespot.ClientId,
					SpircVersion:          "3.2.6",
					Capabilities: &connectpb.Capabilities{
						CanBePlayer:                true,
						RestrictToLocal:            false,
						GaiaEqConnectId:            true,
						SupportsLogout:             true,
						IsObservable:               true,
						VolumeSteps:                VolumeSteps,
						SupportedTypes:             []string{"audio/track"}, // TODO: support episodes
						CommandAcks:                true,                    // TODO: actually send ack
						SupportsRename:             false,
						Hidden:                     false,
						DisableVolume:              false,
						ConnectDisabled:            false,
						SupportsPlaylistV2:         true,
						IsControllable:             true,
						SupportsExternalEpisodes:   false, // TODO: support external episodes
						SupportsSetBackendMetadata: false,
						SupportsTransferCommand:    true, // TODO: actually support transfer command
						SupportsCommandRequest:     true,
						IsVoiceEnabled:             false,
						NeedsFullPlayerState:       false,
						SupportsGzipPushes:         true, // TODO: actually support gzip pushes
						SupportsSetOptionsCommand:  false,
						SupportsHifi:               nil, // TODO: nice to have?
						ConnectCapabilities:        "",
					},
				},
				PlayerState: &connectpb.PlayerState{
					IsSystemInitiated: true,
					// TODO
				},
			},
			IsActive: false,
		}); err != nil {
			return fmt.Errorf("failed initial state put: %w", err)
		}
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/connect/volume") {
		// TODO: update volume value and put state
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/connect/logout") {
		// TODO: we should do this only when using zeroconf (?)
		log.Infof("logging out from %s", s.ap.Username())
		s.Close()
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/cluster") {
		// TODO: detect switching to another device and logout ourselves
	}

	return nil
}

func (s *Session) Connect(creds_ SessionCredentials) (err error) {
	s.stop = make(chan struct{}, 1)

	// init login5
	s.login5 = login5.NewLogin5(s.app.deviceId, s.app.clientToken)

	// connect and authenticate to the accesspoint
	apAddr, err := s.app.resolver.GetAccesspoint()
	if err != nil {
		return fmt.Errorf("failed getting accesspoint from resolver: %w", err)
	}

	s.ap, err = ap.NewAccesspoint(apAddr, s.app.deviceId)
	if err != nil {
		return fmt.Errorf("failed initializing accesspoint: %w", err)
	}

	// choose proper credentials
	switch creds := creds_.(type) {
	case SessionUserPassCredentials:
		if err = s.ap.ConnectUserPass(creds.Username, creds.Password); err != nil {
			return fmt.Errorf("failed authenticating accesspoint with username and password: %w", err)
		}
	case SessionBlobCredentials:
		if err = s.ap.ConnectBlob(creds.Username, creds.Blob); err != nil {
			return fmt.Errorf("failed authenticating accesspoint with blob: %w", err)
		}
	default:
		panic("unknown credentials")
	}

	// authenticate with login5 and get token
	if err = s.login5.Login(&credentialspb.StoredCredential{
		Username: s.ap.Username(),
		Data:     s.ap.StoredCredentials(),
	}); err != nil {
		return fmt.Errorf("failed authenticating with login5: %w", err)
	}

	// initialize spclient
	spAddr, err := s.app.resolver.GetSpclient()
	if err != nil {
		return fmt.Errorf("failed getting spclient from resolver: %w", err)
	}

	s.sp, err = spclient.NewSpclient(spAddr, s.login5.AccessToken(), s.app.deviceId, s.app.clientToken)
	if err != nil {
		return fmt.Errorf("failed initializing spclient: %w", err)
	}

	// initialize dealer
	dealerAddr, err := s.app.resolver.GetDealer()
	if err != nil {
		return fmt.Errorf("failed getting dealer from resolver: %w", err)
	}

	s.dealer, err = dealer.NewDealer(dealerAddr, s.login5.AccessToken())
	if err != nil {
		return fmt.Errorf("failed connecting to dealer: %w", err)
	}

	return nil
}

func (s *Session) Close() {
	s.stop <- struct{}{}
	s.dealer.Close()
	s.ap.Close()
}

func (s *Session) Run() {
	apRecv := s.ap.Receive(ap.PacketTypeProductInfo)
	msgRecv := s.dealer.ReceiveMessage("hm://pusher/v1/connections/", "hm://connect-state/v1/")

	for {
		select {
		case <-s.stop:
			return
		case pkt := <-apRecv:
			if err := s.handleAccesspointPacket(pkt.Type, pkt.Payload); err != nil {
				log.WithError(err).Warn("failed handling accesspoint packet")
			}
		case msg := <-msgRecv:
			if err := s.handleDealerMessage(msg); err != nil {
				log.WithError(err).Warn("failed handling dealer message")
			}
		}
	}
}

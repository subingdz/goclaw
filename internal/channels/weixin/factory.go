package weixin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// FactoryWithAudio returns the same loader factory with the speech-to-text manager attached, so
// a voice note without iLink's own transcription is still turned into text. Registering the plain
// Factory leaves a downloaded SILK blob untranscribed — the media arrives, the words do not.
func FactoryWithAudio(audioMgr *audio.Manager) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

		ch, err := Factory(name, creds, cfg, msgBus, pairingSvc)
		if wc, ok := ch.(*Channel); ok {
			wc.WithAudio(audioMgr)
		}
		return ch, err
	}
}

// Factory builds a WeChat channel from a channel_instances row.
//
// Returning (nil, nil) when no token exists is deliberate: a created-but-unscanned instance
// is a normal state for this channel, and the loader reports it as "enabled but not
// configured" instead of a runtime failure.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

	var c Credentials
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &c); err != nil {
			return nil, fmt.Errorf("weixin: decode credentials: %w", err)
		}
	}
	if c.Token == "" {
		return nil, nil
	}

	var ic InstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("weixin: decode config: %w", err)
		}
	}

	ch, err := New(c, ic, msgBus, pairingSvc)
	if err != nil {
		return nil, err
	}
	ch.SetName(name)
	ch.SetType(channels.TypeWeixin)
	return ch, nil
}

// Activation is one in-flight QR scan. The token only exists after a human scans, so
// activation is deliberately independent of any channel instance: the caller obtains the
// credentials here and then writes them onto an instance.
type Activation struct {
	FlowID         string
	QRCode         string
	QRImageContent string
	Status         string
	Error          string
	BaseURL        string
	CreatedAt      time.Time
	ExpiresAt      time.Time

	cli   *client
	creds Credentials
}

// activationTTL mirrors how long the code stays scannable; beyond it the browser must ask
// for a new one rather than keep polling a dead code.
const activationTTL = 5 * time.Minute

// StartActivation requests a QR code from iLink.
func StartActivation(ctx context.Context, baseURL, proxy string) (*Activation, error) {
	cli, err := newClient(baseURL, "", proxy)
	if err != nil {
		return nil, err
	}
	resp, err := cli.requestQR(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &Activation{
		FlowID:         newClientID(),
		QRCode:         resp.Qrcode,
		QRImageContent: resp.QrcodeImgContent,
		Status:         statusWait,
		BaseURL:        cli.baseURL,
		CreatedAt:      now,
		ExpiresAt:      now.Add(activationTTL),
		cli:            cli,
	}, nil
}

// Poll advances the state machine once. On "confirmed", Credentials() holds the token.
//
// The redirect state is the one subtlety: iLink routes some accounts to a different cell
// mid-scan and only the *polling* follows, so the client is rebuilt against the new host —
// after the host is validated, because it arrives inside a remote response.
func (a *Activation) Poll(ctx context.Context) error {
	if a.cli == nil {
		return fmt.Errorf("weixin: activation %s has no client", a.FlowID)
	}
	status, err := a.cli.pollQR(ctx, a.QRCode)
	if err != nil {
		return err
	}

	switch status.Status {
	case statusScannedRedirect:
		// No host to go to means nothing changed: the reference implementation keeps polling
		// where it is. Falling through to sanitizeBaseURL("") would instead resolve the
		// official root host and drop a cell this account had already been moved off.
		if status.RedirectHost == "" {
			a.Status = status.Status
			break
		}
		next, err := sanitizeBaseURL(status.RedirectHost)
		if err != nil {
			// "error", not "expired": the code may still be live, but no further poll can
			// succeed on a host we refuse to contact, so the flow is terminal now.
			a.Status = "error"
			a.Error = err.Error()
			return err
		}
		if next != a.BaseURL {
			cli, err := newClient(next, "", a.cli.proxy)
			if err != nil {
				a.Status = "error"
				a.Error = err.Error()
				return err
			}
			a.cli, a.BaseURL = cli, next
		}
		a.Status = status.Status
	case statusConfirmed:
		if status.BotToken == "" {
			a.Status = "error"
			a.Error = "weixin: iLink confirmed the scan but returned no bot_token"
			return fmt.Errorf("%s", a.Error)
		}
		if status.BaseURL != "" {
			if cleaned, err := sanitizeBaseURL(status.BaseURL); err == nil {
				a.BaseURL = cleaned
			}
		}
		a.creds = Credentials{Token: status.BotToken, IlinkBotID: status.IlinkBotID, IlinkUserID: status.IlinkUserID}
		a.Status = statusConfirmed
	default:
		// wait, scaned, expired and anything the protocol has not defined yet.
		a.Status = status.Status
	}
	return nil
}

// Credentials returns what a completed activation produced.
func (a *Activation) Credentials() Credentials { return a.creds }

// Expired reports whether the flow outlived its own display window, independently of what
// iLink says about the code.
func (a *Activation) Expired() bool { return time.Now().After(a.ExpiresAt) }

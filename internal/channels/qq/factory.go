package qq

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// FactoryWithAudio returns the same loader factory with the speech-to-text manager attached,
// so inbound voice is transcribed. Registering the plain Factory leaves voice as a downloaded
// but untranscribed file — a working channel with a quieter answer.
func FactoryWithAudio(audioMgr *audio.Manager) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

		ch, err := Factory(name, creds, cfg, msgBus, pairingSvc)
		if qch, ok := ch.(*Channel); ok {
			qch.WithAudio(audioMgr)
		}
		return ch, err
	}
}

// Factory builds a QQ channel from a channel_instances row.
//
// Returning (nil, nil) for absent credentials is deliberate: the instance loader reports
// that as "enabled but not configured yet" instead of a runtime failure, which is the state
// a half-filled wizard entry is actually in.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

	var c Credentials
	if err := json.Unmarshal(creds, &c); err != nil {
		return nil, fmt.Errorf("qq: decode credentials: %w", err)
	}
	if c.AppID == "" || c.AppSecret == "" {
		return nil, nil
	}

	var ic InstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("qq: decode config: %w", err)
		}
	}

	ch, err := New(c, ic, msgBus, pairingSvc)
	if err != nil {
		return nil, err
	}
	ch.SetName(name)
	ch.SetType(channels.TypeQQ)
	return ch, nil
}

package channels

import "errors"

// ErrMediaUnsupported is returned when a channel does not support media attachments.
// Callers (e.g. webhook handler) should either degrade to text-only or return HTTP 501.
var ErrMediaUnsupported = errors.New("channel does not support media attachments")

// MediaBatchCapability describes how a channel can deliver multiple outbound
// attachments from one OutboundMessage.
type MediaBatchCapability struct {
	Supported      bool
	MaxAttachments int
	Grouping       string
}

const (
	MediaBatchGroupingNone          = "none"
	MediaBatchGroupingSingleMessage = "single_message"
	MediaBatchGroupingAlbumChunks   = "album_chunks"
	MediaBatchGroupingOrdered       = "ordered"
)

// mediaCapableTypes lists channel platform types that consume msg.Media in their Send()
// implementation. Verified against adapters:
//   - telegram: internal/channels/telegram/send.go:251
//   - discord:  internal/channels/discord/discord.go:207
//   - whatsapp: internal/channels/whatsapp/outbound.go:68
//   - feishu:   internal/channels/feishu/feishu.go:250
//   - slack:    internal/channels/slack/send.go:80
//   - zalo_personal: internal/channels/zalo/personal/send.go:42
//   - pancake:  internal/channels/pancake/media_handler.go:18
//   - facebook: internal/channels/facebook/facebook.go:205
//   - qq:       internal/channels/qq/media_send.go:146 (base64 upload + msg_type 7)
//   - weixin:   internal/channels/weixin/media_out.go:73 (encrypted-CDN upload + signed ref)
//
// NOT in this list:
//   - zalo_oa: internal/channels/zalo/zalo.go:115 — Send() does NOT consume msg.Media
//
// Both qq and weixin carry attachments as **local paths only**: the upload is made from bytes on
// disk. A remote media_url — what the webhook API hands over — is refused and announced, because
// fetching arbitrary remote URLs on the send path is a second SSRF surface these two adapters do
// not have, unlike telegram/discord/slack which pass the URL to their own servers.
var mediaCapableTypes = map[string]bool{
	TypeTelegram:     true,
	TypeDiscord:      true,
	TypeWhatsApp:     true,
	TypeFeishu:       true,
	TypeSlack:        true,
	TypeZaloPersonal: true,
	TypePancake:      true,
	TypeFacebook:     true,
	TypeQQ:           true,
	TypeWeixin:       true,
}

var mediaBatchCapabilities = map[string]MediaBatchCapability{
	TypeTelegram: {
		Supported:      true,
		MaxAttachments: 10,
		Grouping:       MediaBatchGroupingAlbumChunks,
	},
	TypeDiscord: {
		Supported:      true,
		MaxAttachments: 10,
		Grouping:       MediaBatchGroupingSingleMessage,
	},
	TypeSlack: {
		Supported:      true,
		MaxAttachments: 0,
		Grouping:       MediaBatchGroupingOrdered,
	},
	// One upload and one msg_type=7 message per file: QQ has no multi-attachment envelope for
	// bots, so order is all that can be preserved.
	TypeQQ: {
		Supported:      true,
		MaxAttachments: 0,
		Grouping:       MediaBatchGroupingOrdered,
	},
	// Same shape on iLink: one getuploadurl + one sendmessage per file.
	TypeWeixin: {
		Supported:      true,
		MaxAttachments: 0,
		Grouping:       MediaBatchGroupingOrdered,
	},
}

// IsMediaCapable reports whether the given channel platform type supports media attachments.
// Use Manager.ChannelTypeForName to resolve the type from a channel instance name.
func IsMediaCapable(channelType string) bool {
	return mediaCapableTypes[channelType]
}

// MediaBatchCapabilityFor reports best-known multi-attachment delivery behavior.
// Unknown media-capable channels preserve order by falling back to their Send()
// implementation's existing per-file behavior.
func MediaBatchCapabilityFor(channelType string) MediaBatchCapability {
	if cap, ok := mediaBatchCapabilities[channelType]; ok {
		return cap
	}
	if IsMediaCapable(channelType) {
		return MediaBatchCapability{Supported: true, Grouping: MediaBatchGroupingOrdered}
	}
	return MediaBatchCapability{Grouping: MediaBatchGroupingNone}
}

// Package weixin implements the WeChat personal-account channel over Tencent's official
// iLink (智联) Bot API — the same protocol PicoClaw uses, base host
// https://ilinkai.weixin.qq.com.
//
// What the protocol actually allows, and what that forces in this implementation:
//
//   - The bot cannot start a conversation. Every reply must carry the `context_token`
//     taken from the message it answers, and that token is only produced by an inbound.
//     Anything proactive — cron reminders, heartbeat, an approval push — therefore has no
//     address to send to, and Send says so instead of dropping the text quietly.
//   - Delivery is long-polling `getupdates`, so no public callback URL is needed.
//   - A `-14` session-expired reply means the scan-bound session died: receiving and
//     sending both pause until it is re-activated, rather than hammering the endpoint and
//     inviting WeChat's anti-spam treatment.
//   - Rich media (image/voice/file/video) travels through an AES-encrypted CDN with
//     per-message keys. Inbound is fetched, decrypted and handed to the pipeline; voice uses
//     iLink's own transcription when it has one and GoClaw's ASR chain when it does not.
//     Outbound is the mirror: sign an upload slot with `getuploadurl`, POST the ciphertext,
//     reference what comes back. A spoken reply goes out as a file, because nothing proves
//     this bot can build a voice bar.
package weixin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Wire constants, verbatim from the protocol.
const (
	defaultBaseURL = "https://ilinkai.weixin.qq.com/"

	// channelVersion and clientVersion identify the adapter to iLink. clientVersion packs
	// 2.1.1 as 0x00MMNNPP; a mismatch here is answered with a parameter error, not a
	// version error, so it is not a value to "try other numbers" with.
	channelVersion = "2.1.1"
	clientVersion  = 131329
	appID          = "bot"

	// botType "3" is what the QR endpoint expects for a personal-account bot.
	botType = "3"
)

// Endpoint paths relative to the base URL.
const (
	endpointGetUpdates   = "ilink/bot/getupdates"
	endpointSendMessage  = "ilink/bot/sendmessage"
	endpointGetConfig    = "ilink/bot/getconfig"
	endpointSendTyping   = "ilink/bot/sendtyping"
	endpointGetQRCode    = "ilink/bot/get_bot_qrcode"
	endpointQRStatus     = "ilink/bot/get_qrcode_status"
	endpointGetUploadURL = "ilink/bot/getuploadurl"
)

// errSessionExpired is the code iLink returns when the scan-bound session is dead.
const errSessionExpired = -14

// Message item types inside WeixinMessage.ItemList.
const (
	itemTypeText  = 1
	itemTypeImage = 2
	itemTypeVoice = 3
	itemTypeFile  = 4
	itemTypeVideo = 5
)

const (
	messageTypeBot = 2

	messageStateFinish = 2
)

const (
	typingStatusTyping = 1
	typingStatusCancel = 2
)

// status values of the QR polling endpoint.
const (
	statusWait            = "wait"
	statusScanned         = "scaned"
	statusConfirmed       = "confirmed"
	statusExpired         = "expired"
	statusScannedRedirect = "scaned_but_redirect"
)

// --- Envelopes ---

// baseInfo is attached to every outgoing CGI request.
type baseInfo struct {
	ChannelVersion string `json:"channel_version,omitempty"`
}

// apiStatus is the shared error block. ret/errcode both appear; -14 is the session-expired
// signal and is the only code this package treats specially.
type apiStatus struct {
	Ret     int    `json:"ret,omitempty"`
	Errcode int    `json:"errcode,omitempty"`
	Errmsg  string `json:"errmsg,omitempty"`
}

func (s *apiStatus) code() int {
	if s.Errcode != 0 {
		return s.Errcode
	}
	return s.Ret
}

func (s *apiStatus) failed() bool { return s.code() != 0 }

// --- Messages ---

type textItem struct {
	Text string `json:"text,omitempty"`
}

// cdnMedia describes an encrypted CDN object. Inbound reads encrypt_query_param; outbound fills
// it from the upload response's X-Encrypted-Param header. aes_key on the way out is base64 of the
// *hex string* the upload was given, not of the key bytes — a double encoding, which is exactly
// the shape base64AESKey already accepts on the way in.
type cdnMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AesKey            string `json:"aes_key,omitempty"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
	FullURL           string `json:"full_url,omitempty"`
}

type imageItem struct {
	Media       *cdnMedia `json:"media,omitempty"`
	Aeskey      string    `json:"aeskey,omitempty"`
	MidSize     int64     `json:"mid_size,omitempty"`
	ThumbWidth  int       `json:"thumb_width,omitempty"`
	ThumbHeight int       `json:"thumb_height,omitempty"`
}

type voiceItem struct {
	Media      *cdnMedia `json:"media,omitempty"`
	EncodeType int       `json:"encode_type,omitempty"`
	SampleRate int       `json:"sample_rate,omitempty"`
	Playtime   int       `json:"playtime,omitempty"`
	// Text is the server-side speech-to-text result, which is why voice can be handled
	// without touching the CDN at all.
	Text string `json:"text,omitempty"`
}

type fileItem struct {
	Media    *cdnMedia `json:"media,omitempty"`
	FileName string    `json:"file_name,omitempty"`
	MD5      string    `json:"md5,omitempty"`
	Len      string    `json:"len,omitempty"`
}

type videoItem struct {
	Media      *cdnMedia `json:"media,omitempty"`
	VideoSize  int64     `json:"video_size,omitempty"`
	PlayLength int       `json:"play_length,omitempty"`
}

type messageItem struct {
	ItemType     int    `json:"type,omitempty"`
	CreateTimeMs int64  `json:"create_time_ms,omitempty"`
	UpdateTimeMs int64  `json:"update_time_ms,omitempty"`
	IsCompleted  bool   `json:"is_completed,omitempty"`
	MsgID        string `json:"msg_id,omitempty"`

	TextItem  *textItem  `json:"text_item,omitempty"`
	ImageItem *imageItem `json:"image_item,omitempty"`
	VoiceItem *voiceItem `json:"voice_item,omitempty"`
	FileItem  *fileItem  `json:"file_item,omitempty"`
	VideoItem *videoItem `json:"video_item,omitempty"`
}

// WeixinMessage is both an inbound event and the body of a send.
type WeixinMessage struct {
	Seq          int64  `json:"seq,omitempty"`
	MessageID    int64  `json:"message_id,omitempty"`
	FromUserID   string `json:"from_user_id,omitempty"`
	ToUserID     string `json:"to_user_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	GroupID      string `json:"group_id,omitempty"`
	MessageType  int    `json:"message_type,omitempty"`
	MessageState int    `json:"message_state,omitempty"`

	ItemList     []messageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
}

type getUpdatesReq struct {
	SyncBuf       string   `json:"sync_buf,omitempty"`
	GetUpdatesBuf string   `json:"get_updates_buf,omitempty"`
	BaseInfo      baseInfo `json:"base_info,omitempty"`
}

type getUpdatesResp struct {
	apiStatus
	Msgs                 []WeixinMessage `json:"msgs,omitempty"`
	SyncBuf              string          `json:"sync_buf,omitempty"`
	GetUpdatesBuf        string          `json:"get_updates_buf,omitempty"`
	LongpollingTimeoutMs int             `json:"longpolling_timeout_ms,omitempty"`
}

type sendMessageReq struct {
	Msg      WeixinMessage `json:"msg,omitempty"`
	BaseInfo baseInfo      `json:"base_info,omitempty"`
}

type sendMessageResp struct {
	apiStatus
}

type getConfigReq struct {
	IlinkUserID  string   `json:"ilink_user_id,omitempty"`
	ContextToken string   `json:"context_token,omitempty"`
	BaseInfo     baseInfo `json:"base_info,omitempty"`
}

type getConfigResp struct {
	apiStatus
	TypingTicket string `json:"typing_ticket,omitempty"`
}

type sendTypingReq struct {
	IlinkUserID  string   `json:"ilink_user_id,omitempty"`
	TypingTicket string   `json:"typing_ticket,omitempty"`
	Status       int      `json:"status,omitempty"`
	BaseInfo     baseInfo `json:"base_info,omitempty"`
}

type sendTypingResp struct {
	apiStatus
}

// --- Outbound media ---

// Upload media types for getuploadurl. The protocol enumerates four; VOICE is included here
// because the shape says so, but no implementation is known to send it — the reference client
// defines the constant and never uses it, and outbound audio therefore travels as FILE and
// renders as a file card, not a voice bar.
const (
	uploadMediaImage = 1
	uploadMediaVideo = 2
	uploadMediaFile  = 3
	uploadMediaVoice = 4 // declared by the protocol, unproven in practice
)

type getUploadURLReq struct {
	FileKey     string `json:"filekey"`
	MediaType   int    `json:"media_type"`
	ToUserID    string `json:"to_user_id"`
	RawSize     int    `json:"rawsize"`
	RawFileMD5  string `json:"rawfilemd5"`
	FileSize    int    `json:"filesize"`
	NoNeedThumb bool   `json:"no_need_thumb"`
	// AesKey is the hex of the 16 random bytes used to AES-ECB the payload. The upload
	// service needs it before the bytes arrive, which is why the client generates the key
	// rather than the service handing one back.
	AesKey   string   `json:"aeskey"`
	BaseInfo baseInfo `json:"base_info,omitempty"`
}

type getUploadURLResp struct {
	apiStatus
	// UploadParam is the token for a relative upload: POST the ciphertext to
	// {cdn}/upload?encrypted_query_param={UploadParam}&filekey={filekey}. UploadFullURL, when
	// the service supplies one, is already absolute — and is still host-pinned before use.
	UploadParam   string `json:"upload_param,omitempty"`
	UploadFullURL string `json:"upload_full_url,omitempty"`
}

type qrCodeResponse struct {
	Qrcode string `json:"qrcode"`
	// QrcodeImgContent is the string to encode into the QR image, not an image itself.
	QrcodeImgContent string `json:"qrcode_img_content"`
}

type qrStatusResponse struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token,omitempty"`
	IlinkBotID   string `json:"ilink_bot_id,omitempty"`
	BaseURL      string `json:"baseurl,omitempty"`
	IlinkUserID  string `json:"ilink_user_id,omitempty"`
	RedirectHost string `json:"redirect_host,omitempty"`
}

// --- Derived helpers ---

// renderText turns an item list into the text the pipeline sees. Non-text items become
// explicit placeholders: an agent that cannot see the image must at least know one was sent.
func renderText(items []messageItem) string { return renderItems(items, nil) }

// renderItems is renderText with the attachments that were actually fetched excluded by index.
// Their placeholder would otherwise say "[图片]" next to a tag that says the image arrived, and
// the contradiction is the kind of thing an agent reads as "I did not get it".
func renderItems(items []messageItem, skip map[int]bool) string {
	parts := make([]string, 0, len(items))
	for i, it := range items {
		if skip[i] {
			continue
		}
		switch it.ItemType {
		case itemTypeText:
			if it.TextItem != nil && it.TextItem.Text != "" {
				parts = append(parts, it.TextItem.Text)
			}
		case itemTypeImage:
			parts = append(parts, "[图片]")
		case itemTypeVoice:
			if it.VoiceItem != nil && it.VoiceItem.Text != "" {
				// iLink already transcribes voice; using that text is what makes voice
				// messages useful without downloading a single byte.
				parts = append(parts, it.VoiceItem.Text)
			} else {
				parts = append(parts, "[语音]")
			}
		case itemTypeFile:
			if it.FileItem != nil && it.FileItem.FileName != "" {
				parts = append(parts, "[文件: "+it.FileItem.FileName+"]")
			} else {
				parts = append(parts, "[文件]")
			}
		case itemTypeVideo:
			parts = append(parts, "[视频]")
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// accountKey namespaces persisted state per credential pair, so two weixin instances on one
// gateway cannot share a cursor or reply tokens.
func accountKey(baseURL, token string) string {
	if baseURL == "" && token == "" {
		return "default"
	}
	sum := sha256.Sum256([]byte(baseURL + "|" + token))
	return hex.EncodeToString(sum[:])[:16]
}

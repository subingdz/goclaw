package weixin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	goclawprotocol "github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

const (
	// qrSessionTimeout outlives activationTTL so the flow always reaches a stated reason
	// (expired) before the session is cut from underneath it.
	qrSessionTimeout = 6 * time.Minute
	qrPollInterval   = 2 * time.Second

	// qrMaxPollFails bounds the loop against a network that never recovers. A transient
	// failure must not end a scan that is half-finished on the phone, but an endless retry
	// would look identical to the panel.
	qrMaxPollFails = 5
)

// Terminal reasons, sent as a stable code so the panel can localize them itself. The raw
// error text travels alongside for anything this list does not cover.
const (
	qrReasonExpired = "expired"
	qrReasonTimeout = "timeout"
	qrReasonNetwork = "network"
	qrReasonError   = "error"
)

// cancelEntry wraps a CancelFunc so it can be stored in sync.Map.CompareAndDelete.
type cancelEntry struct {
	cancel context.CancelFunc
}

// qrEvents is everything a QR session needs from its transport, which keeps the state
// machine testable without a websocket client attached.
type qrEvents interface {
	code(pngB64 string)
	status(status string)
	done(success bool, reason, errMsg, botID string)
}

// QRMethods handles weixin.qr.start — turns an iLink QR code into stored credentials.
type QRMethods struct {
	instanceStore  store.ChannelInstanceStore
	loader         *channels.InstanceLoader
	activeSessions sync.Map // instanceID (string) -> *cancelEntry
}

func NewQRMethods(instanceStore store.ChannelInstanceStore, loader ...*channels.InstanceLoader) *QRMethods {
	m := &QRMethods{instanceStore: instanceStore}
	if len(loader) > 0 {
		m.loader = loader[0]
	}
	return m
}

func (m *QRMethods) Register(router *gateway.MethodRouter) {
	router.Register(goclawprotocol.MethodWeixinQRStart, m.handleQRStart)
}

// qrPNGBase64 draws the scan payload as a data-uri-ready base64 PNG.
func qrPNGBase64(content string) (string, error) {
	png, err := qrcode.Encode(content, qrcode.Low, 256)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(png), nil
}

func (m *QRMethods) handleQRStart(ctx context.Context, client *gateway.Client, req *goclawprotocol.RequestFrame) {
	var params struct {
		InstanceID string `json:"instance_id"`
	}
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
	}

	instID, err := uuid.Parse(params.InstanceID)
	if err != nil {
		client.SendResponse(goclawprotocol.NewErrorResponse(req.ID, goclawprotocol.ErrInvalidRequest, "invalid instance_id"))
		return
	}

	// Tenant-scoped by the request context: another tenant's instance id is simply not found.
	inst, err := m.instanceStore.Get(ctx, instID)
	if err != nil || inst.ChannelType != channels.TypeWeixin {
		client.SendResponse(goclawprotocol.NewErrorResponse(req.ID, goclawprotocol.ErrNotFound, "weixin instance not found"))
		return
	}

	var ic InstanceConfig
	if len(inst.Config) > 0 {
		if err := json.Unmarshal(inst.Config, &ic); err != nil {
			client.SendResponse(goclawprotocol.NewErrorResponse(req.ID, goclawprotocol.ErrInvalidRequest, "invalid weixin instance config"))
			return
		}
	}

	qrCtx, cancel := context.WithTimeout(ctx, qrSessionTimeout)
	entry := &cancelEntry{cancel: cancel}

	// One scan in flight per instance: two concurrent QR flows on the same row would both
	// try to write the token, and only the last confirmation would survive.
	if prev, loaded := m.activeSessions.Swap(params.InstanceID, entry); loaded {
		if prevEntry, ok := prev.(*cancelEntry); ok {
			prevEntry.cancel()
		}
	}

	// ACK immediately — code/status/done events arrive asynchronously.
	client.SendResponse(goclawprotocol.NewOKResponse(req.ID, map[string]any{"status": "started"}))

	go m.runQRSession(qrCtx, entry, &clientEmitter{client: client, instanceID: params.InstanceID}, instID, params.InstanceID, ic)
}

func (m *QRMethods) runQRSession(ctx context.Context, entry *cancelEntry,
	ev qrEvents, instanceID uuid.UUID, instanceIDStr string, ic InstanceConfig) {

	defer entry.cancel()
	defer m.activeSessions.CompareAndDelete(instanceIDStr, entry)

	act, err := StartActivation(ctx, ic.BaseURL, ic.Proxy)
	if err != nil {
		slog.Warn("weixin QR: could not request a code", "instance", instanceIDStr, "error", err)
		ev.done(false, qrReasonError, err.Error(), "")
		return
	}

	creds, ok := driveActivation(ctx, act, ev, qrPollInterval)
	if !ok {
		return
	}
	if err := m.persistCredentials(ctx, instanceID, creds); err != nil {
		slog.Warn("weixin QR: token received but not stored", "instance", instanceIDStr, "error", err)
		ev.done(false, qrReasonError, fmt.Sprintf("scan confirmed but the token could not be stored: %v", err), "")
		return
	}

	if m.loader != nil {
		// The channel is long-lived; do not tie its lifetime to this QR session's deadline.
		loadCtx := context.WithoutCancel(ctx)
		if err := m.loader.LoadInstanceByID(loadCtx, instanceID); err != nil {
			slog.Warn("weixin QR: targeted channel load failed", "instance", instanceIDStr, "error", err)
		}
	}

	slog.Info("weixin QR session completed", "instance", instanceIDStr, "flow", act.FlowID, "bot_id", creds.IlinkBotID)
	ev.done(true, "", "", creds.IlinkBotID)
}

// driveActivation displays one activation's code and polls it to a terminal state, emitting
// what the panel needs along the way. It returns the credentials only on a confirmed scan.
func driveActivation(ctx context.Context, act *Activation, ev qrEvents, every time.Duration) (Credentials, bool) {
	// QrcodeImgContent is the string to encode, not an image — iLink hands out the payload
	// and the client draws the code. The panel shows exactly one code per activation because
	// iLink never re-issues it.
	pngB64, err := qrPNGBase64(act.QRImageContent)
	if err != nil {
		slog.Warn("weixin QR: PNG encode failed", "flow", act.FlowID, "error", err)
		ev.done(false, qrReasonError, fmt.Sprintf("could not render the QR code: %v", err), "")
		return Credentials{}, false
	}
	ev.code(pngB64)

	// The code is drawn once; a status change is the only signal that anything is happening.
	seenScaned := false
	fails := 0

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Expired vs. timed out is which clock ran out — the panel says so in its own
			// words, so no text travels with either.
			reason := qrReasonTimeout
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && act.Expired() {
				reason = qrReasonExpired
			}
			ev.done(false, reason, "", "")
			return Credentials{}, false

		case <-ticker.C:
			if act.Expired() {
				ev.done(false, qrReasonExpired, "", "")
				return Credentials{}, false
			}
			if err := act.Poll(ctx); err != nil {
				if act.Status == "error" {
					// Text travels with these two because the reason alone cannot say which
					// host was refused or which call broke.
					ev.done(false, qrReasonError, err.Error(), "")
					return Credentials{}, false
				}
				if fails++; fails >= qrMaxPollFails {
					ev.done(false, qrReasonNetwork, fmt.Sprintf("%d consecutive failures: %v", fails, err), "")
					return Credentials{}, false
				}
				continue
			}
			fails = 0

			switch act.Status {
			case statusConfirmed:
				return act.Credentials(), true
			case statusExpired:
				ev.done(false, qrReasonExpired, "", "")
				return Credentials{}, false
			case statusScannedRedirect:
				// Redirected mid-scan: the code still works, the polling host changed.
			case statusScanned:
				if !seenScaned {
					seenScaned = true
					ev.status(statusScanned)
				}
			default:
				// wait, and any state the protocol has not defined yet.
			}
		}
	}
}

// persistCredentials writes the scanned token onto the instance. The store encrypts the
// blob and merges keys, so only weixin's own fields are touched.
//
// Replacing an existing token is what re-activation means here: iLink binds a token to one
// session, so the old one is already dead (code -14) by the time anybody scans again.
func (m *QRMethods) persistCredentials(ctx context.Context, instanceID uuid.UUID, creds Credentials) error {
	if m.instanceStore == nil {
		return errors.New("weixin: no instance store to write the token to")
	}
	return m.instanceStore.Update(context.WithoutCancel(ctx), instanceID, map[string]any{
		"credentials": map[string]any{
			"token":         creds.Token,
			"ilink_bot_id":  creds.IlinkBotID,
			"ilink_user_id": creds.IlinkUserID,
		},
	})
}

// clientEmitter adapts a websocket client to qrEvents. Every payload carries the instance id
// so a panel with more than one weixin dialog open can route the event to the right one.
type clientEmitter struct {
	client     *gateway.Client
	instanceID string
}

func (e *clientEmitter) code(pngB64 string) {
	e.send(goclawprotocol.EventWeixinQRCode, map[string]any{"png_b64": pngB64})
}

func (e *clientEmitter) status(status string) {
	e.send(goclawprotocol.EventWeixinQRStatus, map[string]any{"status": status})
}

func (e *clientEmitter) done(success bool, reason, errMsg, botID string) {
	payload := map[string]any{"success": success}
	if reason != "" {
		payload["reason"] = reason
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	if botID != "" {
		payload["ilink_bot_id"] = botID
	}
	e.send(goclawprotocol.EventWeixinQRDone, payload)
}

func (e *clientEmitter) send(event string, payload map[string]any) {
	if e.client == nil {
		return
	}
	payload["instance_id"] = e.instanceID
	e.client.SendEvent(goclawprotocol.EventFrame{
		Type:    goclawprotocol.FrameTypeEvent,
		Event:   event,
		Payload: payload,
	})
}

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	imsg "github.com/iammatthias/farfield/apps/switchboard/gen/photon/imessage/v1"
)

// photonClient talks to Photon's iMessage line.
//
// The line speaks gRPC and only gRPC. Photon's protos carry google.api.http
// annotations and their SDK ships an HTTP transport, but that transport targets
// a self-hosted Advanced iMessage Kit server; the shared cloud endpoint answers
// 415 to anything that is not application/grpc. Hence the generated stubs in
// gen/ and the grpc dependency — there is no JSON path to fall back on.
//
// Credentials are two-tier: long-lived project credentials mint a short-lived
// line token (15 minutes) over HTTPS, and that token authenticates each RPC.
// Minting is lazy and cached, so a quiet service holds no live token at all.
type photonClient struct {
	projectID     string
	projectSecret string
	cloudURL      string
	conn          *grpc.ClientConn
	msgs          imsg.MessageServiceClient
	atts          imsg.AttachmentServiceClient
	hc            *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// tokenSkew re-mints a little before expiry so an RPC never races the boundary.
const tokenSkew = 60 * time.Second

// maxAttachment caps a download. Photos and short videos land well inside it;
// the cap is what stops a hostile or corrupt length from exhausting memory.
const maxAttachment = 100 << 20 // 100 MiB

// newPhotonClient dials the line. It returns (nil, nil) when unconfigured —
// switchboard still serves and still records inbound messages, it simply has
// nowhere to reply, which is the right behaviour in tests and on a fresh box.
func newPhotonClient(projectID, projectSecret, address, cloudURL string) (*photonClient, error) {
	if projectID == "" || projectSecret == "" {
		return nil, nil
	}
	c := &photonClient{
		projectID:     projectID,
		projectSecret: projectSecret,
		cloudURL:      strings.TrimRight(cloudURL, "/"),
		hc:            &http.Client{Timeout: 20 * time.Second},
	}
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		// Per-RPC rather than a dial-time header: tokens expire every 15
		// minutes and this connection outlives many of them, so the credential
		// has to be resolved per call.
		grpc.WithPerRPCCredentials(&lineToken{c: c}),
		// Photos are the reason for the raised receive limit — the default 4 MiB
		// would reject a frame from any real camera.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxAttachment)),
	)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	c.msgs = imsg.NewMessageServiceClient(conn)
	c.atts = imsg.NewAttachmentServiceClient(conn)
	return c, nil
}

func (c *photonClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// lineToken supplies the bearer token to every RPC, minting on demand.
type lineToken struct{ c *photonClient }

func (t *lineToken) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	tok, err := t.c.lineToken(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

func (t *lineToken) RequireTransportSecurity() bool { return true }

// tokenResponse is the Spectrum cloud envelope: {"succeed":true,"data":{...}}.
type tokenResponse struct {
	Succeed bool `json:"succeed"`
	Data    struct {
		Type      string `json:"type"`
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
	} `json:"data"`
	Message string `json:"message"`
}

// lineToken returns a valid line token, minting a new one when the cached token
// is missing or close to expiry.
func (c *photonClient) lineToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires.Add(-tokenSkew)) {
		return c.token, nil
	}
	url := c.cloudURL + "/projects/" + c.projectID + "/imessage/tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.projectID, c.projectSecret)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("photon token: %s: %w", resp.Status, err)
	}
	if !out.Succeed || out.Data.Token == "" {
		return "", fmt.Errorf("photon token: %s: %s", resp.Status, out.Message)
	}
	c.token = out.Data.Token
	c.expires = time.Now().Add(time.Duration(out.Data.ExpiresIn) * time.Second)
	return c.token, nil
}

// userAgent identifies switchboard to Photon and to the Cloudflare edge, which
// is choosy about some default client strings.
const userAgent = "farfield-switchboard/1.0"

// dmChatGUID builds the deterministic 1:1 chat id for a handle. Shared lines
// cannot create group chats, and a DM guid needs no server round trip — the
// form is fixed.
func dmChatGUID(handle string) string {
	if handle == "" {
		return ""
	}
	return "any;-;" + handle
}

// SendText replies in a conversation.
func (c *photonClient) SendText(ctx context.Context, chatGUID, text string) error {
	if c == nil {
		return fmt.Errorf("photon line not configured")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	_, err := c.msgs.SendTextMessage(ctx, &imsg.SendTextMessageRequest{
		ChatGuid: chatGUID,
		Text:     text,
	})
	return err
}

// Download pulls an attachment's primary bytes off the line.
//
// The stream interleaves three frame kinds: one header, the primary file's
// chunks, and — for a Live Photo — the companion video's chunks. Only the
// primary is collected: what belongs in a post is the still image, not its
// three-second video.
func (c *photonClient) Download(ctx context.Context, guid string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("photon line not configured")
	}
	stream, err := c.atts.DownloadAttachment(ctx, &imsg.DownloadAttachmentRequest{
		AttachmentGuid: guid,
	})
	if err != nil {
		return nil, err
	}
	var data []byte
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		chunk := frame.GetPrimaryChunk()
		if len(chunk) == 0 {
			continue
		}
		if len(data)+len(chunk) > maxAttachment {
			return nil, fmt.Errorf("attachment %s exceeds %d bytes", guid, maxAttachment)
		}
		data = append(data, chunk...)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("attachment %s returned no bytes", guid)
	}
	return data, nil
}

// SendImage uploads bytes to the line and sends them into a conversation —
// how a generated QR code arrives as a picture rather than a link.
func (c *photonClient) SendImage(ctx context.Context, chatGUID, name string, data []byte) error {
	if c == nil {
		return fmt.Errorf("photon line not configured")
	}
	up, err := c.atts.UploadAttachment(ctx, &imsg.UploadAttachmentRequest{
		FileName: name,
		Data:     data,
	})
	if err != nil {
		return err
	}
	guid := up.GetAttachment().GetGuid()
	if guid == "" {
		return fmt.Errorf("photon upload returned no attachment guid")
	}
	_, err = c.msgs.SendAttachmentMessage(ctx, &imsg.SendAttachmentMessageRequest{
		ChatGuid: chatGUID,
		Attachment: &imsg.AttachmentRef{
			Source:         &imsg.AttachmentRef_AttachmentGuid{AttachmentGuid: guid},
			AttachmentName: &name,
		},
	})
	return err
}

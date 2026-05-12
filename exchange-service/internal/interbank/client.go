package interbank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// HeaderAPIKey is the header name carrying the per-partner X-Api-Key
// token (the key the PARTNER issued to US). Per spec §1.
const HeaderAPIKey = "X-Api-Key"

// ErrNoSuchPartner is returned when an outbound call targets a routing
// number we don't have registered.
var ErrNoSuchPartner = errors.New("interbank: no partner registered for routing number")

// ErrAcceptedTimeout is returned when the partner kept replying 202
// Accepted for longer than the client's polling budget. The caller's
// 2PC state machine should retry the same idempotence key later — the
// partner will eventually return the same final response.
var ErrAcceptedTimeout = errors.New("interbank: partner kept replying 202 Accepted past the polling deadline")

// RemoteError is the error type returned when a partner replies with a
// 4xx/5xx status. The body is captured verbatim so callers can decide
// whether to surface it.
type RemoteError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (e *RemoteError) Error() string {
	bodyPreview := string(e.Body)
	if len(bodyPreview) > 256 {
		bodyPreview = bodyPreview[:256] + "..."
	}
	return fmt.Sprintf("interbank: partner returned HTTP %d %s: %s", e.StatusCode, e.Status, bodyPreview)
}

// ClientOption customises Client behaviour at construction time.
type ClientOption func(*Client)

// WithHTTPClient lets tests inject a stub *http.Client.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.http = h }
}

// WithRetryPolicy sets the polling cadence used when a partner replies
// 202 Accepted. The slice's length is the maximum number of polls; each
// element is the delay BEFORE that poll. Default: 7 polls with backoff
// from 250ms up to 8s (≈18s total).
func WithRetryPolicy(delays []time.Duration) ClientOption {
	return func(c *Client) { c.retryDelays = delays }
}

// WithSleepFunc overrides time.Sleep — wired by tests so retry loops
// can be exercised without real wall clock waits.
func WithSleepFunc(f func(time.Duration)) ClientOption {
	return func(c *Client) { c.sleep = f }
}

// Client is the outbound transport for /interbank messages. One Client
// can talk to any partner in its Registry; routing is by RoutingNumber.
type Client struct {
	registry    *Registry
	http        *http.Client
	retryDelays []time.Duration
	sleep       func(time.Duration)
}

// NewClient builds an outbound client around a Registry.
func NewClient(registry *Registry, opts ...ClientOption) *Client {
	c := &Client{
		registry: registry,
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
		retryDelays: []time.Duration{
			250 * time.Millisecond,
			500 * time.Millisecond,
			1 * time.Second,
			2 * time.Second,
			4 * time.Second,
			4 * time.Second,
			6 * time.Second,
		},
		sleep: time.Sleep,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewIdempotenceKey returns a fresh idempotence key tagged with our own
// routing number. The locally-generated half is a UUID v4 hex string
// (32 chars — well under the protocol's 64-byte cap).
func (c *Client) NewIdempotenceKey() IdempotenceKey {
	return IdempotenceKey{
		RoutingNumber:       c.registry.OwnRoutingNumber(),
		LocallyGeneratedKey: uuid.NewString(),
	}
}

// SendNewTx POSTs a NEW_TX message to the partner that owns
// partnerCode and returns the partner's TransactionVote. On a 202
// Accepted, the same idempotence key is re-POSTed per retryDelays
// until the partner returns a definitive response.
func (c *Client) SendNewTx(ctx context.Context, partnerCode RoutingNumber, key IdempotenceKey, tx *Transaction) (*TransactionVote, error) {
	msg, err := NewMessage(key, tx)
	if err != nil {
		return nil, err
	}
	respBody, err := c.sendEnvelope(ctx, partnerCode, msg)
	if err != nil {
		return nil, err
	}
	if len(respBody) == 0 {
		return nil, fmt.Errorf("interbank: partner returned empty body for NEW_TX (expected TransactionVote)")
	}
	var vote TransactionVote
	if err := json.Unmarshal(respBody, &vote); err != nil {
		return nil, fmt.Errorf("interbank: decoding NEW_TX response: %w", err)
	}
	return &vote, nil
}

// SendCommitTx POSTs a COMMIT_TX message. The protocol body is empty on
// success (204 No Content), so the return value is just error.
func (c *Client) SendCommitTx(ctx context.Context, partnerCode RoutingNumber, key IdempotenceKey, transactionID ForeignBankId) error {
	msg, err := NewMessage(key, CommitTransaction{TransactionID: transactionID})
	if err != nil {
		return err
	}
	_, err = c.sendEnvelope(ctx, partnerCode, msg)
	return err
}

// SendRollbackTx POSTs a ROLLBACK_TX message. Same shape as commit.
func (c *Client) SendRollbackTx(ctx context.Context, partnerCode RoutingNumber, key IdempotenceKey, transactionID ForeignBankId) error {
	msg, err := NewMessage(key, RollbackTransaction{TransactionID: transactionID})
	if err != nil {
		return err
	}
	_, err = c.sendEnvelope(ctx, partnerCode, msg)
	return err
}

// FetchPublicStock GETs the partner's /public-stock list — the catalogue
// of OTC sellers the partner is willing to expose to peer banks. Spec §3.4.
func (c *Client) FetchPublicStock(ctx context.Context, partnerCode RoutingNumber) (PublicStocksResponse, error) {
	var out PublicStocksResponse
	if err := c.doJSON(ctx, partnerCode, http.MethodGet, "/public-stock", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateNegotiation POSTs an OtcOffer to the seller's bank /negotiations
// and returns the seller-minted ForeignBankId for the new negotiation.
// Spec §3.2 — buyer's bank initiates by POSTing here.
func (c *Client) CreateNegotiation(ctx context.Context, partnerCode RoutingNumber, offer OtcOffer) (ForeignBankId, error) {
	var out ForeignBankId
	if err := c.doJSON(ctx, partnerCode, http.MethodPost, "/negotiations", offer, &out); err != nil {
		return ForeignBankId{}, err
	}
	return out, nil
}

// GetNegotiation reads the partner's authoritative copy of a negotiation
// at /negotiations/{routingNumber}/{id}. Spec §3.3.
func (c *Client) GetNegotiation(ctx context.Context, partnerCode RoutingNumber, routing RoutingNumber, id string) (*OtcNegotiation, error) {
	var out OtcNegotiation
	path := fmt.Sprintf("/negotiations/%d/%s", int(routing), id)
	if err := c.doJSON(ctx, partnerCode, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateNegotiation PUTs a counter-offer to the partner's
// /negotiations/{routingNumber}/{id} and returns the partner's updated
// view of the negotiation. Spec §3.3.
func (c *Client) UpdateNegotiation(ctx context.Context, partnerCode RoutingNumber, routing RoutingNumber, id string, offer OtcOffer) (*OtcNegotiation, error) {
	var out OtcNegotiation
	path := fmt.Sprintf("/negotiations/%d/%s", int(routing), id)
	if err := c.doJSON(ctx, partnerCode, http.MethodPut, path, offer, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseNegotiation DELETEs /negotiations/{routingNumber}/{id} on the
// partner. The partner is expected to flip is_ongoing → false; we send
// the same DELETE for both buyer-side and seller-side closes. Spec §3.3.
func (c *Client) CloseNegotiation(ctx context.Context, partnerCode RoutingNumber, routing RoutingNumber, id string) error {
	path := fmt.Sprintf("/negotiations/%d/%s", int(routing), id)
	return c.doJSON(ctx, partnerCode, http.MethodDelete, path, nil, nil)
}

// doJSON is the plain-HTTP analogue of sendEnvelope used by the §3 OTC
// REST surface. It does NOT poll 202 Accepted — those replies are only
// defined for the /interbank envelope endpoint. 204 No Content is
// accepted as a successful empty response.
func (c *Client) doJSON(ctx context.Context, partnerCode RoutingNumber, method, path string, in any, out any) error {
	partner := c.registry.Lookup(partnerCode)
	if partner == nil {
		return fmt.Errorf("%w: %d", ErrNoSuchPartner, partnerCode)
	}

	var bodyReader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("interbank: marshalling %s %s body: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	endpoint := partner.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("interbank: building %s %s request: %w", method, endpoint, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderAPIKey, partner.OutboundKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("interbank: %s %s: %w", method, endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("interbank: reading response body from %s %s: %w", method, endpoint, readErr)
	}

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil || len(respBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("interbank: decoding %s %s response: %w", method, endpoint, err)
		}
		return nil
	default:
		return &RemoteError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       respBody,
		}
	}
}

// sendEnvelope is the workhorse: serialises the envelope, POSTs to
// {partner.BaseURL}/interbank with the partner's OutboundKey, and
// retries the SAME request (same idempotence key, same body) while the
// partner returns 202 Accepted. The protocol guarantees the partner
// will replay the cached final response, so we just keep asking.
func (c *Client) sendEnvelope(ctx context.Context, partnerCode RoutingNumber, msg *Message) ([]byte, error) {
	partner := c.registry.Lookup(partnerCode)
	if partner == nil {
		return nil, fmt.Errorf("%w: %d", ErrNoSuchPartner, partnerCode)
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("interbank: marshalling envelope: %w", err)
	}
	endpoint := partner.BaseURL + "/interbank"

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("interbank: building request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set(HeaderAPIKey, partner.OutboundKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("interbank: POST %s: %w", endpoint, err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("interbank: reading response body from %s: %w", endpoint, readErr)
		}

		switch resp.StatusCode {
		case http.StatusOK:
			return respBody, nil
		case http.StatusNoContent:
			return nil, nil
		case http.StatusAccepted:
			if attempt >= len(c.retryDelays) {
				slog.Warn("interbank: partner kept replying 202 Accepted past polling budget",
					"partner", partnerCode, "attempts", attempt, "message_type", msg.MessageType)
				return nil, ErrAcceptedTimeout
			}
			slog.Debug("interbank: 202 Accepted, will re-POST same idempotence key",
				"partner", partnerCode, "attempt", attempt+1, "delay", c.retryDelays[attempt])
			c.sleep(c.retryDelays[attempt])
			continue
		default:
			return nil, &RemoteError{
				StatusCode: resp.StatusCode,
				Status:     resp.Status,
				Body:       respBody,
			}
		}
	}
}

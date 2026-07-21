// Package telegram is the Telegram ChannelAdapter for the Legion IM gateway. It
// ingests messages via getUpdates long polling and delivers replies via
// sendMessage, keeping the gateway fully outbound (no public webhook needed).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/stardust/legion-agent/internal/gateway"
)

const defaultPollTimeoutSeconds = 30

// Adapter implements gateway.ChannelAdapter for Telegram.
type Adapter struct {
	token       string
	baseURL     string // https://api.telegram.org/bot<token>
	pollTimeout int
	client      *http.Client
	offset      int64
	logger      *slog.Logger
}

// New builds a Telegram adapter from platform config. A missing token is a loud
// error. Satisfies gateway's factory signature.
func New(cfg gateway.PlatformConfig) (gateway.ChannelAdapter, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	timeout := cfg.PollTimeoutSeconds
	if timeout <= 0 {
		timeout = defaultPollTimeoutSeconds
	}
	return &Adapter{
		token:       cfg.Token,
		baseURL:     "https://api.telegram.org/bot" + cfg.Token,
		pollTimeout: timeout,
		client:      &http.Client{Timeout: time.Duration(timeout+10) * time.Second},
		logger:      slog.Default(),
	}, nil
}

// newForTest builds an adapter pointed at a stub base URL for tests.
func newForTest(token, baseURL string, pollTimeout int) *Adapter {
	if pollTimeout <= 0 {
		pollTimeout = 1
	}
	return &Adapter{
		token:       token,
		baseURL:     baseURL,
		pollTimeout: pollTimeout,
		client:      &http.Client{Timeout: 5 * time.Second},
		logger:      slog.Default(),
	}
}

// Platform returns "telegram".
func (a *Adapter) Platform() string { return "telegram" }

// Close is a no-op; the HTTP client needs no teardown.
func (a *Adapter) Close() error { return nil }

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

type tgUpdatesResponse struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

// Start runs the getUpdates long-poll loop until ctx is cancelled, pushing each
// text message to inbound. The update offset advances past processed updates so
// each is delivered once. A transient HTTP error is not fatal — it is surfaced
// as a returned error only on ctx cancellation; per-iteration failures back off
// briefly and retry.
func (a *Adapter) Start(ctx context.Context, inbound chan<- gateway.InboundMessage) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		updates, err := a.getUpdates(ctx)
		if err != nil {
			// Transient: log then back off and retry unless cancelled.
			a.logger.Warn("telegram getUpdates failed; backing off", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for _, u := range updates {
			if u.UpdateID >= a.offset {
				a.offset = u.UpdateID + 1
			}
			if u.Message.Text == "" {
				continue
			}
			msg := gateway.InboundMessage{
				Platform:   "telegram",
				ChatID:     strconv.FormatInt(u.Message.Chat.ID, 10),
				UserID:     strconv.FormatInt(u.Message.From.ID, 10),
				Text:       u.Message.Text,
				ReceivedAt: time.Now(),
			}
			select {
			case inbound <- msg:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (a *Adapter) getUpdates(ctx context.Context) ([]tgUpdate, error) {
	url := fmt.Sprintf("%s/getUpdates?timeout=%d&offset=%d", a.baseURL, a.pollTimeout, a.offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read telegram getUpdates response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram getUpdates status %s: %s", resp.Status, string(data))
	}
	var parsed tgUpdatesResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("telegram getUpdates decode: %w", err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("telegram getUpdates: ok=false")
	}
	return parsed.Result, nil
}

// Send delivers text to the target chat via sendMessage.
func (a *Adapter) Send(ctx context.Context, target gateway.DeliveryTarget, text string) error {
	body, err := json.Marshal(map[string]any{"chat_id": target.ChatID, "text": text})
	if err != nil {
		return fmt.Errorf("telegram sendMessage encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Only used to enrich an error that is already being returned, so a read
		// failure here costs detail and nothing else — same reasoning as
		// internal/adapter/http_maas.go.
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("telegram sendMessage status %s: %s", resp.Status, string(data))
	}
	return nil
}

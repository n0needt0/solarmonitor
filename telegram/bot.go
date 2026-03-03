package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const apiBase = "https://api.telegram.org/bot"

// Bot handles Telegram bot communication
type Bot struct {
	token       string
	chatID      string
	pollTimeout int
	httpClient  *http.Client

	// Command handler
	handler CommandHandler

	// Control
	mu       sync.Mutex
	running  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	lastSent map[string]time.Time // Rate limiting for alerts
}

// CommandHandler processes incoming commands
type CommandHandler interface {
	HandleStatus() string
	HandleStop() string
	HandleStart() string
	HandleUp() string
	HandleDown() string
	HandleStats() string
	HandleCharge() string
	HandleDischarge() string
	HandleReboot() string
	HandleCycle() string
}

// Update represents a Telegram update
type Update struct {
	UpdateID int `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message represents a Telegram message
type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
}

// Chat represents a Telegram chat
type Chat struct {
	ID int64 `json:"id"`
}

// getUpdatesResponse is the API response for getUpdates
type getUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// NewBot creates a new Telegram bot
func NewBot(token, chatID string, pollTimeout int) *Bot {
	return &Bot{
		token:       token,
		chatID:      chatID,
		pollTimeout: pollTimeout,
		httpClient: &http.Client{
			Timeout: time.Duration(pollTimeout+10) * time.Second,
		},
		stopCh:   make(chan struct{}),
		lastSent: make(map[string]time.Time),
	}
}

// SetHandler sets the command handler
func (b *Bot) SetHandler(h CommandHandler) {
	b.handler = h
}

// Start begins the polling loop
func (b *Bot) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("bot already running")
	}
	b.running = true
	b.mu.Unlock()

	if b.token == "" {
		slog.Info("telegram bot disabled (no token)")
		return nil
	}

	slog.Info("telegram bot starting", "chat_id", b.chatID)

	b.wg.Add(1)
	go b.pollLoop(ctx)

	return nil
}

// Stop stops the bot
func (b *Bot) Stop() {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return
	}
	b.running = false
	b.mu.Unlock()

	close(b.stopCh)
	b.wg.Wait()
	slog.Info("telegram bot stopped")
}

// pollLoop continuously polls for updates
func (b *Bot) pollLoop(ctx context.Context) {
	defer b.wg.Done()

	var offset int

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		default:
		}

		updates, err := b.getUpdates(offset)
		if err != nil {
			slog.Warn("telegram getUpdates failed", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			offset = update.UpdateID + 1
			b.handleUpdate(update)
		}
	}
}

// getUpdates fetches updates from Telegram
func (b *Bot) getUpdates(offset int) ([]Update, error) {
	url := fmt.Sprintf("%s%s/getUpdates?offset=%d&timeout=%d",
		apiBase, b.token, offset, b.pollTimeout)

	resp, err := b.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result getUpdatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API error")
	}

	return result.Result, nil
}

// handleUpdate processes a single update
func (b *Bot) handleUpdate(update Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message

	// Only respond to configured chat
	chatIDStr := fmt.Sprintf("%d", msg.Chat.ID)
	if chatIDStr != b.chatID {
		slog.Debug("ignoring message from unknown chat", "chat_id", msg.Chat.ID)
		return
	}

	if b.handler == nil {
		return
	}

	var response string

	switch msg.Text {
	case "/status":
		response = b.handler.HandleStatus()
	case "/stop":
		response = b.handler.HandleStop()
	case "/start":
		response = b.handler.HandleStart()
	case "/up":
		response = b.handler.HandleUp()
	case "/down":
		response = b.handler.HandleDown()
	case "/stats":
		response = b.handler.HandleStats()
	case "/charge":
		response = b.handler.HandleCharge()
	case "/discharge":
		response = b.handler.HandleDischarge()
	case "/reboot":
		response = b.handler.HandleReboot()
	case "/cycle":
		response = b.handler.HandleCycle()
	case "/help":
		response = FormatHelp()
	default:
		return
	}

	slog.Info("telegram_cmd", "cmd", msg.Text)

	if response != "" {
		if err := b.SendMessage(response); err != nil {
			slog.Error("failed to send telegram response", "error", err)
		}
	}
}

// SendMessage sends a message to the configured chat
func (b *Bot) SendMessage(text string) error {
	if b.token == "" || b.chatID == "" {
		return nil
	}

	url := fmt.Sprintf("%s%s/sendMessage", apiBase, b.token)

	payload := map[string]any{
		"chat_id": b.chatID,
		"text":    text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := b.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API error: %s", string(respBody))
	}

	return nil
}

// Alert sends an alert message with rate limiting
func (b *Bot) Alert(alertType, message string) error {
	b.mu.Lock()
	lastSent, exists := b.lastSent[alertType]
	b.mu.Unlock()

	// Rate limit: one alert per type per hour
	if exists && time.Since(lastSent) < time.Hour {
		slog.Debug("alert rate limited", "type", alertType)
		return nil
	}

	if err := b.SendMessage(message); err != nil {
		return err
	}

	b.mu.Lock()
	b.lastSent[alertType] = time.Now()
	b.mu.Unlock()

	slog.Info("telegram_alert", "type", alertType)
	return nil
}

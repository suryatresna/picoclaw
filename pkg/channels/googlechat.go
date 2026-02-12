package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"

	"github.com/suryatresna/picoclaw/pkg/bus"
	"github.com/suryatresna/picoclaw/pkg/config"
)

// GoogleChatChannel implements the Channel interface for Google Chat.
//
// It works in two modes:
//   - Synchronous (HTTP endpoint): receives interaction events from Google Chat
//     via an HTTP POST endpoint. Google Chat sends DeprecatedEvent payloads when
//     users message the bot.
//   - Asynchronous (Chat API): sends messages back to spaces using the
//     Google Chat API with service account credentials.
//
// Required configuration (config.GoogleChatConfig):
//   - ServiceAccountFile: path to the service account JSON key file
//   - ProjectID: GCP project ID (used for verification token, optional)
//   - Port: HTTP port for the webhook endpoint (default "8080")
//   - Endpoint: HTTP path for the webhook (default "/googlechat")
//   - VerificationToken: token from Google Chat API configuration (optional, for request verification)
//   - AllowFrom: list of allowed sender IDs
type GoogleChatChannel struct {
	*BaseChannel
	config       config.GoogleChatConfig
	chatService  *chat.Service
	httpServer   *http.Server
	placeholders sync.Map // spaceID -> message name (for editing)
	stopThinking sync.Map // spaceID -> chan struct{}
}

// NewGoogleChatChannel creates a new Google Chat channel. It initializes the
// Chat API service using service account credentials for sending messages
// asynchronously, and sets up an HTTP endpoint for receiving interaction events.
func NewGoogleChatChannel(cfg config.GoogleChatConfig, msgBus *bus.MessageBus) (*GoogleChatChannel, error) {
	base := NewBaseChannel("googlechat", cfg, msgBus, cfg.AllowFrom)

	svc, err := newChatService(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create google chat service: %w", err)
	}

	return &GoogleChatChannel{
		BaseChannel:  base,
		config:       cfg,
		chatService:  svc,
		placeholders: sync.Map{},
		stopThinking: sync.Map{},
	}, nil
}

// newChatService creates an authenticated *chat.Service using either:
//  1. A service account key file (if ServiceAccountFile is set), or
//  2. Application Default Credentials (ADC).
func newChatService(cfg config.GoogleChatConfig) (*chat.Service, error) {
	bgCtx := context.Background()

	var opts []option.ClientOption

	if cfg.ServiceAccountFile != "" {
		data, err := os.ReadFile(cfg.ServiceAccountFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read service account file: %w", err)
		}

		creds, err := google.CredentialsFromJSON(bgCtx, data, "https://www.googleapis.com/auth/chat.bot")
		if err != nil {
			return nil, fmt.Errorf("failed to parse service account credentials: %w", err)
		}

		opts = append(opts, option.WithCredentials(creds))
	}
	// If no service account file, fall back to Application Default Credentials.

	svc, err := chat.NewService(bgCtx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create chat service: %w", err)
	}

	return svc, nil
}

// Start begins listening for Google Chat interaction events on the configured
// HTTP endpoint. Events are dispatched to handleEvent for processing.
func (c *GoogleChatChannel) Start(ctx context.Context) error {
	port := c.config.Port
	if port == "" {
		port = "8080"
	}

	endpoint := c.config.Endpoint
	if endpoint == "" {
		endpoint = "/googlechat"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(endpoint, c.webhookHandler)

	c.httpServer = &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	c.setRunning(true)
	log.Printf("Google Chat bot listening on :%s%s", port, endpoint)

	go func() {
		if err := c.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Google Chat HTTP server error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		c.Stop(context.Background())
	}()

	return nil
}

// Stop gracefully shuts down the HTTP server and marks the channel as not running.
func (c *GoogleChatChannel) Stop(ctx context.Context) error {
	log.Println("Stopping Google Chat bot...")
	c.setRunning(false)

	if c.httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := c.httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Google Chat HTTP server shutdown error: %v", err)
			return err
		}
	}

	return nil
}

// Send delivers an outbound message to a Google Chat space. It stops any
// active thinking animation, attempts to update the placeholder message,
// and falls back to creating a new message if the update fails.
func (c *GoogleChatChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("google chat bot not running")
	}

	// Stop thinking animation.
	if stop, ok := c.stopThinking.Load(msg.ChatID); ok {
		close(stop.(chan struct{}))
		c.stopThinking.Delete(msg.ChatID)
	}

	spaceName := msg.ChatID // expected format: "spaces/XXXX"

	// Try to update the placeholder message first.
	if pName, ok := c.placeholders.Load(msg.ChatID); ok {
		c.placeholders.Delete(msg.ChatID)

		updateMsg := &chat.Message{Text: msg.Content}
		_, err := c.chatService.Spaces.Messages.Update(pName.(string), updateMsg).
			UpdateMask("text").Do()
		if err == nil {
			return nil
		}
		log.Printf("Failed to update placeholder message, sending new: %v", err)
		// Fall through to create a new message.
	}

	chatMsg := &chat.Message{Text: msg.Content}
	_, err := c.chatService.Spaces.Messages.Create(spaceName, chatMsg).Do()
	if err != nil {
		return fmt.Errorf("failed to send google chat message: %w", err)
	}

	return nil
}

// webhookHandler is the HTTP handler for incoming Google Chat interaction events.
func (c *GoogleChatChannel) webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read request body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var event chat.DeprecatedEvent
	if err := json.Unmarshal(body, &event); err != nil {
		log.Printf("Failed to parse event: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Optional: verify the request token.
	if c.config.VerificationToken != "" && event.Token != c.config.VerificationToken {
		log.Printf("Invalid verification token")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch event.Type {
	case "ADDED_TO_SPACE":
		c.handleAddedToSpace(w, &event)
	case "MESSAGE":
		c.handleMessageEvent(w, &event)
	case "REMOVED_FROM_SPACE":
		log.Printf("Bot removed from space: %s", event.Space.Name)
		w.WriteHeader(http.StatusOK)
	case "CARD_CLICKED":
		// Card interactions can be handled here if needed.
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// handleAddedToSpace responds with a greeting when the bot is added to a space.
func (c *GoogleChatChannel) handleAddedToSpace(w http.ResponseWriter, event *chat.DeprecatedEvent) {
	spaceType := ""
	if event.Space != nil {
		spaceType = event.Space.Type
	}

	var greeting string
	if spaceType == "DM" {
		greeting = "Hello! I'm ready to help. Send me a message to get started."
	} else {
		greeting = "Thanks for adding me! Mention me to get started."
	}

	resp := map[string]string{"text": greeting}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleMessageEvent processes incoming MESSAGE events. It extracts sender info,
// message content and attachments, sends a thinking indicator, and dispatches
// the message through the bus.
func (c *GoogleChatChannel) handleMessageEvent(w http.ResponseWriter, event *chat.DeprecatedEvent) {
	if event.Message == nil || event.User == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	user := event.User
	message := event.Message
	space := event.Space

	senderID := user.Name // format: "users/XXXX"
	if user.Name != "" {
		senderID = fmt.Sprintf("%s", user.Name)
	}

	spaceName := ""
	spaceType := ""
	if space != nil {
		spaceName = space.Name
		spaceType = space.Type
	}

	content := ""
	mediaPaths := []string{}

	if message.Text != "" {
		content = message.Text
	}

	// Handle attachments if present.
	if message.Attachment != nil {
		for _, att := range message.Attachment {
			if att.DownloadUri != "" {
				mediaPaths = append(mediaPaths, att.DownloadUri)
				if content != "" {
					content += "\n"
				}
				contentType := att.ContentType
				if contentType == "" {
					contentType = "file"
				}
				content += fmt.Sprintf("[%s: %s]", contentType, att.ContentName)
			}
		}
	}

	// Strip bot mention from the message text in spaces (not DMs).
	if spaceType != "DM" && content != "" {
		content = stripBotMention(content, message.Annotations)
		content = strings.TrimSpace(content)
	}

	if content == "" {
		content = "[empty message]"
	}

	log.Printf("Google Chat message from %s in %s: %s...", senderID, spaceName, truncateString(content, 50))

	// Send a synchronous "thinking" response immediately.
	resp := map[string]string{"text": "Thinking... 💭"}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	// Start async thinking animation by updating the message.
	// The synchronous response creates a message; we create a new one via API
	// to have a reference we can update.
	go func() {
		thinkMsg := &chat.Message{Text: "Thinking... 💭"}
		created, err := c.chatService.Spaces.Messages.Create(spaceName, thinkMsg).Do()
		if err != nil {
			log.Printf("Failed to create thinking message: %v", err)
			return
		}

		c.placeholders.Store(spaceName, created.Name)

		stopChan := make(chan struct{})
		c.stopThinking.Store(spaceName, stopChan)

		go func(msgName string, stop <-chan struct{}) {
			dots := []string{".", "..", "..."}
			emotes := []string{"💭", "🤔", "☁️"}
			i := 0
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					i++
					text := fmt.Sprintf("Thinking%s %s", dots[i%len(dots)], emotes[i%len(emotes)])
					updateMsg := &chat.Message{Text: text}
					_, err := c.chatService.Spaces.Messages.Update(msgName, updateMsg).
						UpdateMask("text").Do()
					if err != nil {
						log.Printf("Failed to update thinking message: %v", err)
						return
					}
				}
			}
		}(created.Name, stopChan)
	}()

	metadata := map[string]string{
		"message_name": message.Name,
		"user_name":    user.Name,
		"display_name": user.DisplayName,
		"space_type":   spaceType,
		"is_group":     fmt.Sprintf("%t", spaceType != "DM"),
	}

	c.HandleMessage(senderID, spaceName, content, mediaPaths, metadata)
}

// stripBotMention removes the bot's @mention from the message text.
// In Google Chat, when a user mentions the bot in a space, the mention
// is included in the text. We strip it so the downstream handler gets
// clean text.
func stripBotMention(text string, annotations []*chat.Annotation) string {
	if len(annotations) == 0 {
		return text
	}

	// Process annotations in reverse order so indices remain valid.
	// We only strip USER_MENTION annotations of type BOT.
	type removal struct {
		start, length int
	}
	var removals []removal

	for _, ann := range annotations {
		if ann.Type == "USER_MENTION" && ann.UserMention != nil {
			if ann.UserMention.User != nil && ann.UserMention.User.Type == "BOT" {
				removals = append(removals, removal{
					start:  int(ann.StartIndex),
					length: int(ann.Length),
				})
			}
		}
	}

	// Sort descending by start index to preserve positions.
	for i := 0; i < len(removals); i++ {
		for j := i + 1; j < len(removals); j++ {
			if removals[j].start > removals[i].start {
				removals[i], removals[j] = removals[j], removals[i]
			}
		}
	}

	runes := []rune(text)
	for _, rm := range removals {
		end := rm.start + rm.length
		if rm.start < len(runes) {
			if end > len(runes) {
				end = len(runes)
			}
			runes = append(runes[:rm.start], runes[end:]...)
		}
	}

	return string(runes)
}

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"recallweave/internal/events"
	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
)

type ConversationEventReader interface {
	ConversationExists(context.Context, uint) (bool, error)
	ValidateCursor(context.Context, uint, uint64) error
	List(context.Context, uint, uint64, int) ([]types.ConversationEvent, error)
}

type EventsHandler struct {
	reader            ConversationEventReader
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	writeTimeout      time.Duration
}

func NewEventsHandler(reader ConversationEventReader) *EventsHandler {
	return &EventsHandler{reader: reader, pollInterval: 500 * time.Millisecond, heartbeatInterval: 15 * time.Second, writeTimeout: 10 * time.Second}
}

func MountConversationEvents(router *gin.Engine, handler *EventsHandler) {
	router.GET("/api/conversations/:conversation_id/events", handler.Stream)
}

var eventKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
var eventCursorPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func eventCursor(c *gin.Context) (uint64, error) {
	header := strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	query, hasQuery := c.Request.URL.Query()["after"]
	parse := func(value string) (uint64, error) {
		if !eventCursorPattern.MatchString(value) {
			return 0, fmt.Errorf("invalid event cursor")
		}
		return strconv.ParseUint(value, 10, 64)
	}
	if header != "" {
		return parse(header)
	}
	var after uint64
	if hasQuery {
		if len(query) != 1 {
			return 0, fmt.Errorf("invalid event cursor")
		}
		fromQuery, err := parse(query[0])
		if err != nil {
			// 从 0 开始
			return 0, err
		}
		after = fromQuery
	}
	return after, nil
}

func (h *EventsHandler) Stream(c *gin.Context) {
	owner, ok := conversationIDFromPath(c)
	if !ok {
		return
	}
	// sse 从哪开始
	after, err := eventCursor(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid event cursor"})
		return
	}
	if h == nil || h.reader == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "conversation events are unavailable"})
		return
	}
	ctx := c.Request.Context()
	// 判断 conversation 是否存在
	exists, err := h.reader.ConversationExists(ctx, owner)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}
	if err := h.reader.ValidateCursor(ctx, owner, after); err != nil {
		if errors.Is(err, events.ErrInvalidCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "event cursor does not belong to this conversation"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate event cursor"})
		}
		return
	}
	load := func() ([]types.ConversationEvent, error) {
		batch, err := h.reader.List(ctx, owner, after, 100)
		if err != nil {
			return nil, err
		}
		previous := after
		for _, event := range batch {
			if event.ConversationID != owner || event.ID <= previous || !eventKindPattern.MatchString(event.Kind) || !utf8.Valid(event.Data) || !json.Valid(event.Data) {
				return nil, fmt.Errorf("invalid typesd event")
			}
			previous = event.ID
		}
		return batch, nil
	}
	batch, err := load()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation events"})
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	controller := http.NewResponseController(c.Writer)
	defer controller.SetWriteDeadline(time.Time{})
	write := func(body []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := controller.SetWriteDeadline(time.Now().Add(h.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		defer controller.SetWriteDeadline(time.Time{})
		if _, err := c.Writer.Write(body); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write([]byte("retry: 3000\n\n")); err != nil {
		return
	}
	poll := time.NewTicker(h.pollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(h.heartbeatInterval)
	defer heartbeat.Stop()
	for {
		for _, event := range batch {
			var compact bytes.Buffer
			if err := json.Compact(&compact, event.Data); err != nil {
				return
			}
			frame := fmt.Appendf(nil, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Kind, compact.Bytes())
			if err := write(frame); err != nil {
				return
			}
			after = event.ID
		}
		// 100了 重装
		if len(batch) == 100 {
			batch, err = load()
			if err != nil {
				return
			}
			continue
		}
		batch = nil
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
		// 到点 装载
		case <-poll.C:
			batch, err = load()
			if err != nil {
				return
			}
		}
	}
}

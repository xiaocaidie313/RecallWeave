package ask

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type AskHandler struct {
	agent *Agent
}

func NewAskHandler(agent *Agent) *AskHandler {
	return &AskHandler{agent: agent}
}

type askRequest struct {
	Question string `json:"question" binding:"required"`
}

// Ask 是用户问答的入口。handler 只做 JSON 绑定和状态码，
// 往下传的是普通字符串，agent 不认识 gin。
func (h *AskHandler) Ask(c *gin.Context) {

	conversationID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid conversation id",
		})
		return
	}

	var req askRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	question := strings.TrimSpace(req.Question)
	if question == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "question is empty",
		})
		return
	}

	answer, err := h.agent.Ask(c.Request.Context(), question, uint(conversationID))
	if err != nil {
		if errors.Is(err, ErrNoModel) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, answer)
}

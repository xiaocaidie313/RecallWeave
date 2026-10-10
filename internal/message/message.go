// agent 间传递信息的schema 

type Message struct {
	ID int64 "json:"id"`
	AgentID string `json:"agent_id"`
	Content string `json:"content"`
	Topic string `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
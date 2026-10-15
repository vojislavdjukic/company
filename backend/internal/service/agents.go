package service

import (
	"errors"
	"time"
)

// ErrAgentNameTaken means the account already uses the requested display name.
var ErrAgentNameTaken = errors.New("agent name is already in use")

// AgentMessage is one persisted user prompt or Codex response in an agent chat.
type AgentMessage struct {
	ID        int64     `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

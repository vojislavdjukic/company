package service

import "errors"

// ErrAgentNameTaken means the account already uses the requested display name.
var ErrAgentNameTaken = errors.New("agent name is already in use")

// Package server provides a minimal configurable server.
package server

import (
	"errors"
	"fmt"
)

// Config holds server configuration.
type Config struct {
	Host string `json:"host" validate:"required"`
	Port int    `json:"port"`
}

// Server wraps a Config with lifecycle state.
// It embeds Config directly so callers can access Host and Port without indirection.
type Server struct {
	Config       // embedded — exercises GoField.is_embedded
	ready  bool  // unexported field — exercises is_exported=false on GoField
}

// New creates a Server, returning an error if the config is invalid.
// Exercises: multiple return types (*Server, error), (T, error) idiom.
func New(cfg Config) (*Server, error) {
	if cfg.Host == "" {
		return nil, errors.New("host required")
	}
	return &Server{Config: cfg}, nil
}

// Addr returns the host:port address string.
// Exercises: pointer receiver (*Server), non-empty receiver_type / receiver_name.
func (s *Server) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// Validate checks the config fields and returns any validation errors.
// Exercises: named return, multiple return types (bool, error).
func (s *Server) Validate() (bool, error) {
	if s.Host == "" {
		return false, errors.New("host is empty")
	}
	if s.Port <= 0 {
		return false, fmt.Errorf("invalid port: %d", s.Port)
	}
	return true, nil
}

// shutdown performs internal cleanup.
// Exercises: unexported method — is_exported=false.
func (s *Server) shutdown() {
	s.ready = false
}

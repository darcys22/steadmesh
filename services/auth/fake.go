package auth

import (
	"context"
	"sync"
)

// Fake is an in-memory Authenticator for tests.
type Fake struct {
	mu         sync.Mutex
	seats      map[string]Seat
	controller string
	console    string
}

// NewFake returns a Fake accepting controllerToken on the internal API.
func NewFake(controllerToken string) *Fake {
	return &Fake{seats: map[string]Seat{}, controller: controllerToken}
}

// AddSeat makes token authenticate as seat.
func (f *Fake) AddSeat(token string, seat Seat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seats[token] = seat
}

// SetConsole makes token authenticate on the console API.
func (f *Fake) SetConsole(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.console = token
}

// Seat implements Authenticator.
func (f *Fake) Seat(_ context.Context, token string) (Seat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.seats[token]
	if !ok {
		return Seat{}, ErrUnauthenticated
	}
	return s, nil
}

// Controller implements Authenticator.
func (f *Fake) Controller(_ context.Context, token string) error {
	if token == "" || token != f.controller {
		return ErrUnauthenticated
	}
	return nil
}

// Console implements Authenticator.
func (f *Fake) Console(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.console == "":
		return ErrForbidden
	case token == "" || token != f.console:
		return ErrUnauthenticated
	}
	return nil
}

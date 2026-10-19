package runtimeapi

import "time"

// PoolMember is an independently authenticated ordinary seat, not a child task.
type PoolMember struct {
	SeatID        string     `json:"seat_id"`
	SeatKey       string     `json:"seat_key"`
	Team          string     `json:"team"`
	Template      string     `json:"template"`
	State         string     `json:"state"`
	RetainedUntil *time.Time `json:"retained_until,omitempty"`
	PurgedAt      *time.Time `json:"purged_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}
type PoolSpawnRequest struct {
	Template  string `json:"template"`
	RequestID string `json:"request_id"`
}

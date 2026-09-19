package olcrtc

import (
	"expvar"
	"sync"
	"time"
)

// Room health published for monitoring.
//
// The pool already knows everything an operator wants: which room is carrying
// traffic, how often each one has failed, and how long the current one is
// benched for. None of it was reachable from outside the process — Rooms()
// existed but had no caller — so an olcrtc outage was invisible until users
// complained.
//
// expvar is the seam because app/metrics already serves /debug/vars and folds
// every published var into its reply. That means no new listener, no new port
// and no change to the metrics handler: publishing here is enough for the node
// agent to read it.
//
// Registration is per inbound tag, so a config with several olcrtc inbounds
// reports each separately rather than silently exporting whichever registered
// last.

var olcrtcServers sync.Map // tag (string) -> *Server

func init() {
	expvar.Publish("olcrtc", expvar.Func(olcrtcVars))
}

// registerForVars makes this server's room pool visible at /debug/vars. Safe to
// call with an empty tag — a nameless inbound is still worth reporting, and the
// empty key simply means "the untagged one".
func (s *Server) registerForVars() { olcrtcServers.Store(s.tag, s) }

type roomVars struct {
	ID string `json:"id"`
	Up bool   `json:"up"`

	Failures int    `json:"failures"`
	LastErr  string `json:"lastErr,omitempty"`

	// Unix seconds, 0 when the event has not happened yet. Deliberately not
	// RFC3339: the consumer turns these straight into Prometheus gauges, and a
	// number needs no timezone handling on either side.
	LastAttemptUnix   int64 `json:"lastAttemptUnix"`
	LastHealthyUnix   int64 `json:"lastHealthyUnix"`
	CooldownUntilUnix int64 `json:"cooldownUntilUnix"`
}

type inboundVars struct {
	Tag   string     `json:"tag"`
	Rooms []roomVars `json:"rooms"`
}

func olcrtcVars() interface{} {
	out := []inboundVars{}
	olcrtcServers.Range(func(key, value interface{}) bool {
		tag, _ := key.(string)
		srv, ok := value.(*Server)
		if !ok || srv == nil {
			return true
		}
		states := srv.Rooms()
		rooms := make([]roomVars, 0, len(states))
		for _, st := range states {
			rooms = append(rooms, roomVars{
				ID:                st.id,
				Up:                st.up,
				Failures:          st.failures,
				LastErr:           st.lastError,
				LastAttemptUnix:   unixOrZero(st.lastAttempt),
				LastHealthyUnix:   unixOrZero(st.lastHealthy),
				CooldownUntilUnix: unixOrZero(st.cooldownUntil),
			})
		}
		out = append(out, inboundVars{Tag: tag, Rooms: rooms})
		return true
	})
	return out
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

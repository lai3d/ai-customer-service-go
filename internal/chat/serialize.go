package chat

import (
	"context"
	"sync"
)

// conversationLocks serialises whole turns per conversation.
//
// A turn reads history, calls the model and writes a reply, and those steps are only
// coherent together. Without this, two overlapping requests on one conversation
// interleave: the second one's user message and assistant reply land between the first
// one's write and its history read, so the first sends the model a conversation ending in
// somebody else's answer -- and because passages are attached only to a trailing user
// message, its retrieved material is dropped on the floor at the same time. Two browser
// tabs are enough to cause it. Found by an external review; TestOverlappingTurnsOnOne
// ConversationDoNotInterleave reproduces it.
//
// It is a channel rather than a sync.Mutex so a caller whose client has already gone away
// stops waiting instead of holding a place in the queue.
//
// The map is bounded by the number of conversations with a request *in flight*, not by
// the number of conversations ever seen: the entry is reference-counted and deleted when
// the last holder leaves. A plain map keyed by conversation id would be the memory leak
// this codebase avoids everywhere else.
//
// Single process only. Two replicas mean two lock tables, and a conversation load-balanced
// across both can still interleave -- the same honest limit as the ticket cap. Postgres
// advisory locks on the conversation id would be the real thing.
type conversationLocks struct {
	mu      sync.Mutex
	holders map[string]*lockHolder
}

type lockHolder struct {
	slot chan struct{}
	refs int
}

func newConversationLocks() *conversationLocks {
	return &conversationLocks{holders: make(map[string]*lockHolder)}
}

// acquire blocks until this conversation is free, or until ctx is done. The returned
// release must be called exactly once, and only when err is nil.
func (l *conversationLocks) acquire(ctx context.Context, conversationID string) (func(), error) {
	l.mu.Lock()
	holder, ok := l.holders[conversationID]
	if !ok {
		holder = &lockHolder{slot: make(chan struct{}, 1)}
		l.holders[conversationID] = holder
	}
	holder.refs++
	l.mu.Unlock()

	release := func() {
		l.mu.Lock()
		holder.refs--
		if holder.refs == 0 {
			delete(l.holders, conversationID)
		}
		l.mu.Unlock()
	}

	select {
	case holder.slot <- struct{}{}:
		return func() {
			<-holder.slot
			release()
		}, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// inFlight is the number of conversations currently holding or waiting for a lock. For
// tests: it must return to zero, or the map leaks.
func (l *conversationLocks) inFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.holders)
}

// InFlightConversations exposes the lock table's size for tests in this package's
// external test file. It is not part of the service's behaviour.
func InFlightConversations(s *Service) int { return s.locks.inFlight() }

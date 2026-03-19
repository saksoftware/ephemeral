package session

import (
	"fmt"
	"sort"
)

const maxUsersInList = 100

// requestParticipants sends a cached list of users that have been seen in the given view.
// For group views it returns the union over all child chats.
func (e *engine) requestParticipants(viewName string) {
	if e.identityCache == nil {
		e.eventsChan <- SurfaceUpdate{Type: "CHAT_USERS_UPDATE", Payload: []Participant{}}
		return
	}

	// Resolve view -> relevant chat names.
	relevantChats := make(map[string]struct{})
	var view *RoomSpec
	for i := range e.prefs.Views {
		if e.prefs.Views[i].Name == viewName {
			view = &e.prefs.Views[i]
			break
		}
	}

	if view == nil {
		// Fallback: if unknown view, just try to treat it as a single chat name.
		if viewName != "" {
			relevantChats[viewName] = struct{}{}
		}
	} else if view.IsGroup {
		for _, child := range view.Children {
			relevantChats[child] = struct{}{}
		}
	} else if view.Name != "" {
		relevantChats[view.Name] = struct{}{}
	}

	if len(relevantChats) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "CHAT_USERS_UPDATE", Payload: []Participant{}}
		return
	}

	// identityCache cache is keyed by pubkey, and stores (nick, chat, shortPubKey).
	usersByPubKey := make(map[string]Participant)
	for _, pk := range e.identityCache.Keys() {
		ctx, ok := e.identityCache.Get(pk)
		if !ok {
			continue
		}
		if _, ok := relevantChats[ctx.chat]; !ok {
			continue
		}
		if ctx.nick == "" || ctx.shortPubKey == "" {
			// Still include it, but avoid blank entries.
			usersByPubKey[pk] = Participant{
				PubKey:      pk,
				Nick:        fmt.Sprintf("%s-%s", pk[:4], pk[4:8]),
				ShortPubKey: ctx.shortPubKey,
				Chat:        ctx.chat,
				LastMsgAt:   ctx.lastMsgAt,
			}
			continue
		}
		usersByPubKey[pk] = Participant{
			PubKey:      pk,
			Nick:        ctx.nick,
			ShortPubKey: ctx.shortPubKey,
			Chat:        ctx.chat,
			LastMsgAt:   ctx.lastMsgAt,
		}
	}

	out := make([]Participant, 0, len(usersByPubKey))
	for _, u := range usersByPubKey {
		out = append(out, u)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Nick == out[j].Nick {
			return out[i].ShortPubKey < out[j].ShortPubKey
		}
		return out[i].Nick < out[j].Nick
	})

	if len(out) > maxUsersInList {
		out = out[:maxUsersInList]
	}

	e.eventsChan <- SurfaceUpdate{Type: "CHAT_USERS_UPDATE", Payload: out}
}

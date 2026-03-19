package session

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mmcloughlin/geohash"
	"github.com/nbd-wtf/go-nostr"
)

// Chat/Group RoomSpec

func (e *engine) joinChats(payload string) {
	chatNames := strings.Fields(payload)
	if len(chatNames) == 0 {
		return
	}

	var addedChats []string
	var existingChats []string

outer:
	for _, name := range chatNames {
		if geohash.Validate(name) != nil {
			normalizedName, err := normalizeAndValidateChatName(name)
			if err != nil {
				e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: err.Error()}
				continue outer
			}
			if utf8.RuneCountInString(normalizedName) > maxChatNameLen {
				e.eventsChan <- SurfaceUpdate{
					Type:    "ERROR",
					Content: fmt.Sprintf("Chat name '%s' is too long (max %d chars).", normalizedName, maxChatNameLen),
				}
				continue outer
			}
			if len(normalizedName) == 0 {
				continue outer
			}
			name = normalizedName
		}

		for _, v := range e.prefs.Views {
			if !v.IsGroup && v.Name == name {
				existingChats = append(existingChats, name)
				continue outer
			}
		}

		newView := RoomSpec{Name: name, IsGroup: false}
		e.prefs.Views = append(e.prefs.Views, newView)
		addedChats = append(addedChats, name)
	}

	switch {
	case len(addedChats) > 0:
		active := addedChats[0]
		e.setActiveView(active)
		e.updateAllSubscriptions()
	case len(existingChats) > 0:
		var content string
		if len(existingChats) == 1 {
			content = fmt.Sprintf("You are already in the '%s' chat.", existingChats[0])
		} else {
			content = fmt.Sprintf("You are already in all specified chats: %s.", strings.Join(existingChats, ", "))
		}
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: content}
	}
}

func (e *engine) createGroup(payload string) {
	existingChats := make(map[string]struct{})
	for _, view := range e.prefs.Views {
		if !view.IsGroup {
			existingChats[view.Name] = struct{}{}
		}
	}

	rawMembers := strings.Split(payload, ",")
	validMembers := make([]string, 0)
	notFoundChats := make([]string, 0)
	seenMembers := make(map[string]struct{})

	for _, member := range rawMembers {
		trimmedMember := strings.TrimSpace(member)
		if trimmedMember == "" {
			continue
		}

		if _, seen := seenMembers[trimmedMember]; seen {
			continue
		}
		seenMembers[trimmedMember] = struct{}{}

		if _, exists := existingChats[trimmedMember]; exists {
			validMembers = append(validMembers, trimmedMember)
		} else {
			notFoundChats = append(notFoundChats, trimmedMember)
		}
	}

	if len(notFoundChats) > 0 {
		e.eventsChan <- SurfaceUpdate{
			Type:    "ERROR",
			Content: fmt.Sprintf("Cannot create group. The following chats were not found: %s", strings.Join(notFoundChats, ", ")),
		}
		return
	}

	if len(validMembers) < 2 {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "A group requires at least two unique, existing chats."}
		return
	}

	sort.Strings(validMembers)

	name := groupName(validMembers)

	for _, view := range e.prefs.Views {
		if view.Name == name {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Group with these chats already exists: '%s'", name)}
			return
		}
	}

	newView := RoomSpec{Name: name, IsGroup: true, Children: validMembers}
	e.prefs.Views = append(e.prefs.Views, newView)
	e.prefs.ActiveViewName = name
	e.persistPrefs()

	e.sendStateUpdate(false)
	e.updateAllSubscriptions()
}

func (e *engine) leaveChat(chatName string) {
	var newViews []RoomSpec
	for _, view := range e.prefs.Views {
		if !view.IsGroup && view.Name == chatName {
			continue
		}
		newViews = append(newViews, view)
	}

	finalViews := make([]RoomSpec, 0, len(newViews))
	for _, view := range newViews {
		if !view.IsGroup {
			finalViews = append(finalViews, view)
			continue
		}

		var newChildren []string
		for _, child := range view.Children {
			if child != chatName {
				newChildren = append(newChildren, child)
			}
		}

		if len(newChildren) < 2 {
			continue
		}
		view.Children = newChildren
		finalViews = append(finalViews, view)
	}

	e.prefs.Views = finalViews
	if e.prefs.ActiveViewName == chatName {
		e.prefs.ActiveViewName = ""
	}
	e.persistPrefs()
	e.sendStateUpdate(false)
	e.updateAllSubscriptions()

	delete(e.chatKeys, chatName)
}

func (e *engine) deleteGroup(groupName string) {
	var newViews []RoomSpec
	for _, view := range e.prefs.Views {
		if view.Name != groupName {
			newViews = append(newViews, view)
		}
	}
	e.prefs.Views = newViews
	if e.prefs.ActiveViewName == groupName {
		e.prefs.ActiveViewName = ""
	}
	e.persistPrefs()
	e.sendStateUpdate(false)
	e.updateAllSubscriptions()
}

func (e *engine) deleteView(viewName string) {
	if viewName == "" {
		activeView := e.getActiveView()
		if activeView == nil {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Cannot delete: there is no active chat."}
			return
		}
		viewName = activeView.Name
	}

	var viewToDelete *RoomSpec
	for i := range e.prefs.Views {
		if e.prefs.Views[i].Name == viewName {
			viewToDelete = &e.prefs.Views[i]
			break
		}
	}

	if viewToDelete == nil {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Chat or group '%s' not found.", viewName)}
		return
	}

	if viewToDelete.IsGroup {
		e.deleteGroup(viewName)
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Group '%s' deleted.", viewName)}
	} else {
		e.leaveChat(viewName)
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Left chat '%s'.", viewName)}
	}
}

// Settings

func (e *engine) setNick(nick string) {
	nick = strings.TrimSpace(nick)
	e.prefs.Nick = nick

	if nick != "" {
		e.n = nick
		e.eventsChan <- SurfaceUpdate{
			Type:    "STATUS",
			Content: fmt.Sprintf("Nick set to: %s", e.n),
		}
		for name, session := range e.chatKeys {
			session.nick = e.n
			session.customNick = true
			e.chatKeys[name] = session
		}
	} else {
		e.n = npubToTokiPona(e.pk)
		for name, session := range e.chatKeys {
			session.nick = npubToTokiPona(session.pubKey)
			session.customNick = false
			e.chatKeys[name] = session
		}
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Nick has been cleared."}
	}

	e.persistPrefs()
	e.sendStateUpdate(false)
}

func (e *engine) setPoW(difficultyStr string) {
	difficulty, err := strconv.Atoi(strings.TrimSpace(difficultyStr))
	if err != nil {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Invalid PoW difficulty: '%s'. Must be a number.", difficultyStr)}
		return
	}

	if difficulty < 0 {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "PoW difficulty cannot be negative."}
		return
	}

	activeView := e.getActiveView()
	if activeView == nil {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Cannot set PoW: no active chat/group."}
		return
	}

	for i := range e.prefs.Views {
		if e.prefs.Views[i].Name == activeView.Name {
			e.prefs.Views[i].PoW = difficulty
			break
		}
	}

	e.persistPrefs()
	e.sendStateUpdate(false)

	if difficulty > 0 {
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("PoW difficulty for %s set to %d.", activeView.Name, difficulty)}
	} else {
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("PoW disabled for %s.", activeView.Name)}
	}
}

// Read-only & Completions

func (e *engine) listChats() {
	if len(e.prefs.Views) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: "You are not in any chats. Use /join <chat_name> to join one."}
		return
	}

	var builder strings.Builder
	builder.WriteString("Available chats and groups:\n")
	for _, view := range e.prefs.Views {
		if view.IsGroup {
			builder.WriteString(fmt.Sprintf(" - %s (Group)\n", view.Name))
		} else {
			builder.WriteString(fmt.Sprintf(" - %s\n", view.Name))
		}
	}
	e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: builder.String()}
}

func (e *engine) getActiveChat() {
	activeView := e.getActiveView()
	var content string
	if activeView != nil {
		content = fmt.Sprintf("Current active chat/group is: %s", activeView.Name)
	} else {
		content = "There is no active chat/group."
	}
	e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: content}
}

func (e *engine) getHelp() {
	helpText := "COMMANDS:\n" +
		"* /join <chat1> [chat2]... - Joins one or more chats. (Alias: /j)\n" +
		"* /set [name|names...] - Without args: shows active chat. With one name: activates a chat/group. With multiple names: creates a group. (Alias: /s)\n" +
		"* /list - Lists all your chats and groups. (Alias: /l)\n" +
		"* /del [name] - Deletes/leaves a chat or group. If no name, deletes the active one. (Alias: /d, /leave)\n" +
		"* /nick [new_nick] - Sets or clears your nickname. (Alias: /n)\n" +
		"* /pow [number] - Sets Proof-of-Work difficulty for the active chat/group. 0 to disable. (Alias: /p)\n" +
		"* /relay [<num>|url1...] - List, remove (#), or add anchor relays. (Alias: /r)\n" +
		"* /block [@nick] - Blocks a user. Without nick, lists blocked users. (Alias: /b)\n" +
		"* /unblock [<num>|@nick|pubkey] - Unblocks a user. Without args, lists blocked users. (Alias: /ub)\n" +
		"* /filter [word|regex|<num>] - Adds a filter. Without args, lists filters. With number, toggles off/on. (Alias: /f)\n" +
		"* /unfilter [<num>] - Removes a filter by number. Without args, clears all. (Alias: /uf)\n" +
		"* /mute [word|regex|<num>] - Adds a mute. Without args, lists mutes. With number, toggles off/on. (Alias: /m)\n" +
		"* /unmute [<num>] - Removes a mute by number. Without args, clears all. (Alias: /um)\n" +
		"* /follow - Toggle auto-scrolling new messages to the bottom.\n" +
		"* /clear - Clears the Messages window (display only). (Alias: /c)\n" +
		"* /quit - Exits the application. (Alias: /q)"

	e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: helpText}
}

func (e *engine) handleNickCompletion(prefix string) {
	prefix = strings.TrimPrefix(prefix, "@")
	var entries []string

	activeView := e.getActiveView()
	if activeView == nil {
		e.eventsChan <- SurfaceUpdate{Type: "NICK_COMPLETION_RESULT", Payload: []string{}}
		return
	}

	relevantChats := make(map[string]struct{})
	if activeView.IsGroup {
		for _, child := range activeView.Children {
			relevantChats[child] = struct{}{}
		}
	} else {
		relevantChats[activeView.Name] = struct{}{}
	}

	for _, key := range e.identityCache.Keys() {
		if value, ok := e.identityCache.Get(key); ok {
			if _, isActiveChat := relevantChats[value.chat]; isActiveChat {
				if strings.HasPrefix(value.nick, prefix) {
					entries = append(entries, fmt.Sprintf("@%s#%s ", value.nick, value.shortPubKey))
				}
			}
		}
	}

	sort.Strings(entries)
	if len(entries) > 10 {
		entries = entries[:10]
	}

	e.eventsChan <- SurfaceUpdate{Type: "NICK_COMPLETION_RESULT", Payload: entries}
}

// Core State Primitives

func (e *engine) setActiveView(name string) {
	prev := e.prefs.ActiveViewName

	viewExists := false
	var view *RoomSpec
	for i := range e.prefs.Views {
		if e.prefs.Views[i].Name == name {
			viewExists = true
			view = &e.prefs.Views[i]
			break
		}
	}

	if !viewExists {
		e.eventsChan <- SurfaceUpdate{
			Type:    "ERROR",
			Content: fmt.Sprintf("Chat or group '%s' not found.", name),
		}
		return
	}

	// Enter on the same 1:1 chat in the list while already there: new ephemeral identity.
	// Only when a session already exists (not on cold start: empty chatKeys + same active view).
	shouldRefreshSubs := prev != name
	clearMessagePane := false
	if !view.IsGroup && prev == name {
		if _, had := e.chatKeys[name]; had {
			delete(e.chatKeys, name)
			shouldRefreshSubs = true
			clearMessagePane = true
			e.discardOrderedStream("chat:" + name)
		}
	}

	if !view.IsGroup {
		if _, exists := e.chatKeys[name]; !exists {
			sk := nostr.GeneratePrivateKey()
			pk, _ := nostr.GetPublicKey(sk)

			nick := e.prefs.Nick
			custom := false
			if nick == "" {
				nick = npubToTokiPona(pk)
			} else {
				custom = true
			}

			e.chatKeys[name] = roomIdentity{
				privKey:    sk,
				pubKey:     pk,
				nick:       nick,
				customNick: custom,
			}
		}
	}

	// Update active view BEFORE refreshing subscriptions
	e.prefs.ActiveViewName = name
	e.persistPrefs()
	e.sendStateUpdate(clearMessagePane)

	// Switching chats or rotating identity: refresh subscriptions for backlog.
	if shouldRefreshSubs {
		go e.forceRefreshSubscriptions()
	}
}

func (e *engine) getActiveView() *RoomSpec {
	for i := range e.prefs.Views {
		if e.prefs.Views[i].Name == e.prefs.ActiveViewName {
			return &e.prefs.Views[i]
		}
	}
	if len(e.prefs.Views) > 0 {
		return &e.prefs.Views[0]
	}
	return nil
}

// Helpers

func (e *engine) sendStateUpdate(clearMessagePane bool) {
	activeIdx := -1
	for i := range e.prefs.Views {
		if e.prefs.Views[i].Name == e.prefs.ActiveViewName {
			activeIdx = i
			break
		}
	}
	if activeIdx == -1 && len(e.prefs.Views) > 0 {
		activeIdx = 0
		e.prefs.ActiveViewName = e.prefs.Views[0].Name
	}

	state := LayoutSnapshot{
		Views:            e.prefs.Views,
		ActiveViewIndex:  activeIdx,
		Nick:             e.n,
		ShortPubKey:      "",
		ClearMessagePane: clearMessagePane,
	}

	if len(e.prefs.Views) == 0 || activeIdx == -1 {
		e.eventsChan <- SurfaceUpdate{Type: "STATE_UPDATE", Payload: state}
		return
	}

	if e.prefs.Nick != "" {
		state.Nick = e.prefs.Nick
	} else {
		v := e.prefs.Views[activeIdx]
		if v.IsGroup {
			state.Nick = npubToTokiPona(e.pk)
		} else if s, ok := e.chatKeys[v.Name]; ok && s.nick != "" {
			state.Nick = s.nick
		} else {
			state.Nick = npubToTokiPona(e.pk)
		}
	}

	// Determine which pubkey identity we currently use for the active view,
	// then send its short prefix to the UI.
	v := e.prefs.Views[activeIdx]
	if v.IsGroup {
		if len(e.pk) >= 4 {
			state.ShortPubKey = e.pk[:4]
		} else {
			state.ShortPubKey = e.pk
		}
	} else if s, ok := e.chatKeys[v.Name]; ok && len(s.pubKey) > 0 {
		if len(s.pubKey) >= 4 {
			state.ShortPubKey = s.pubKey[:4]
		} else {
			state.ShortPubKey = s.pubKey
		}
	} else {
		if len(e.pk) >= 4 {
			state.ShortPubKey = e.pk[:4]
		} else {
			state.ShortPubKey = e.pk
		}
	}

	e.eventsChan <- SurfaceUpdate{Type: "STATE_UPDATE", Payload: state}
}

func (e *engine) persistPrefs() {
	if err := e.prefs.save(); err != nil {
		log.Printf("Error saving prefs: %v", err)
		e.eventsChan <- SurfaceUpdate{
			Type:    "ERROR",
			Content: fmt.Sprintf("Failed to save configuration: %v", err),
		}
	}
}

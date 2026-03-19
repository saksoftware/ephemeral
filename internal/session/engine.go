package session

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/nbd-wtf/go-nostr"
)

type engine struct {
	// Identity & Config
	sk       string // Secret key
	pk       string // Public key
	n        string // Global nick
	prefs    *profile
	chatKeys map[string]roomIdentity

	// TUI I/O
	actionsChan <-chan InboundIntent
	eventsChan  chan<- SurfaceUpdate

	// Client Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Relay State
	relays                    map[string]*relayConn
	relaysMu                  sync.Mutex // Protects relays and lastSubscriptionRelayURLs
	lastSubscriptionRelayURLs []string   // desired relay URLs for active subscription (sorted)

	// Event Processing State
	seenCache     *lru.Cache[string, bool]
	seenCacheMu   sync.Mutex // Protects seenCache
	identityCache *lru.Cache[string, participantRow]
	orderBuf      map[string][]queuedFrame
	orderTimers   map[string]*time.Timer
	orderMu       sync.Mutex // Protects orderBuf, orderTimers

	// Relay Discovery State
	scoutLedger       *relayLedger
	verifyFailCache   *lru.Cache[string, bool]
	verifying         map[string]struct{}
	verifyingMu       sync.Mutex // Protects verifying
	activeDiscoveries int32
	updateSubTimer    *time.Timer
	updateSubMu       sync.Mutex // Protects updateSubTimer

	// Avoid duplicate STATUS lines in Logs (listen vs post-switch refresh).
	subStatusMu      sync.Mutex
	lastListenLogKey string

	// Moderation State
	filtersCompiled []matchRule
	mutesCompiled   []matchRule
}

func (e *engine) resetSeenCache() {
	seenCache, err := lru.New[string, bool](seenCacheSize)
	if err != nil {
		// If cache recreation fails, keep the old one to avoid a crash.
		log.Printf("Failed to recreate seen cache: %v", err)
		return
	}

	e.seenCacheMu.Lock()
	e.seenCache = seenCache
	e.seenCacheMu.Unlock()
}

// forceRefreshSubscriptions clears the seen cache and forces all relays to
// re-subscribe with a fresh Since timestamp, making the relay send us
// historical messages again. This is used when switching chats so the UI
// can display message history.
func (e *engine) forceRefreshSubscriptions() {
	e.resetSeenCache()

	activeView := e.getActiveView()
	activeChats := make(map[string]struct{})
	if activeView != nil {
		if activeView.IsGroup {
			for _, child := range activeView.Children {
				activeChats[child] = struct{}{}
			}
		} else if activeView.Name != "" {
			activeChats[activeView.Name] = struct{}{}
		}
	}

	if len(activeChats) == 0 {
		return
	}

	desiredRelayToChats := make(map[string][]string)
	for chat := range activeChats {
		relayURLs := e.getRelayPoolForChat(chat)
		for _, url := range relayURLs {
			found := false
			for _, existingChat := range desiredRelayToChats[url] {
				if existingChat == chat {
					found = true
					break
				}
			}
			if !found {
				desiredRelayToChats[url] = append(desiredRelayToChats[url], chat)
			}
		}
	}

	e.updateRelaySubscriptionsWithRefresh(desiredRelayToChats, true)

	n := len(desiredRelayToChats)
	if n > 0 && activeView != nil {
		e.emitRelayListenStatus(activeView, n, true)
	}
}

// emitRelayListenStatus writes one line to the TUI Logs panel.
// isChatSwitch: user just changed chat — always show; also updates dedupe key
// so a follow-up updateAllSubscriptions with the same relay set does not repeat.
func (e *engine) emitRelayListenStatus(view *RoomSpec, nRelays int, isChatSwitch bool) {
	if view == nil || nRelays <= 0 {
		return
	}
	key := view.Name + "|" + strconv.Itoa(nRelays)
	e.subStatusMu.Lock()
	if !isChatSwitch && key == e.lastListenLogKey {
		e.subStatusMu.Unlock()
		return
	}
	e.lastListenLogKey = key
	e.subStatusMu.Unlock()

	var msg string
	if isChatSwitch {
		if view.IsGroup {
			msg = fmt.Sprintf("Group %q — loading history (%d relays)", view.Name, nRelays)
		} else {
			msg = fmt.Sprintf("#%s — loading history (%d relays)", strings.TrimPrefix(view.Name, "#"), nRelays)
		}
	} else {
		if view.IsGroup {
			msg = fmt.Sprintf("Group %q — listening (%d relays)", view.Name, nRelays)
		} else {
			msg = fmt.Sprintf("#%s — listening (%d relays)", strings.TrimPrefix(view.Name, "#"), nRelays)
		}
	}
	select {
	case e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: msg}:
	default:
	}
}

// historyLookbackSeconds is the subscription "since" window (Nostr unix seconds).
func (e *engine) historyLookbackSeconds() nostr.Timestamp {
	m := e.prefs.HistoryLookbackMinutes
	if m <= 0 {
		m = defaultHistoryMin
	}
	if m > maxHistoryMin {
		m = maxHistoryMin
	}
	return nostr.Timestamp(m * 60)
}

func NewEngine(actions <-chan InboundIntent, events chan<- SurfaceUpdate) (*engine, error) {
	cfg, err := loadProfile()
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	if cfg.BlockedUsers == nil {
		cfg.BlockedUsers = []blockedPeer{}
	}

	// Remove legacy DM-prefixed views from prefs
	cleanViews := make([]RoomSpec, 0, len(cfg.Views))
	for _, v := range cfg.Views {
		if !strings.HasPrefix(v.Name, "DM-") {
			cleanViews = append(cleanViews, v)
		}
	}
	if len(cleanViews) != len(cfg.Views) {
		cfg.Views = cleanViews
		// Reset active view if it was a DM chat
		if strings.HasPrefix(cfg.ActiveViewName, "DM-") {
			cfg.ActiveViewName = ""
		}
	}

	seenCache, err := lru.New[string, bool](seenCacheSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create seen cache: %w", err)
	}

	idCache, err := lru.New[string, participantRow](identityCacheCapacity)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity cache: %w", err)
	}

	verifyFailCache, err := lru.New[string, bool](2000)
	if err != nil {
		return nil, fmt.Errorf("failed to create verify fail cache: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	eng := &engine{
		prefs:           cfg,
		actionsChan:     actions,
		eventsChan:      events,
		relays:          make(map[string]*relayConn),
		seenCache:       seenCache,
		identityCache:   idCache,
		chatKeys:        make(map[string]roomIdentity),
		orderBuf:        make(map[string][]queuedFrame),
		orderTimers:     make(map[string]*time.Timer),
		verifying:       make(map[string]struct{}),
		verifyFailCache: verifyFailCache,
		ctx:             ctx,
		cancel:          cancel,
	}

	if err := eng.loadDiscoveredRelayStore(); err != nil {
		return nil, fmt.Errorf("failed to load relay store: %w", err)
	}

	eng.rebuildRegexCaches()

	if cfg.Nick != "" {
		eng.n = cfg.Nick
	}

	return eng, nil
}

func (e *engine) Run() {
	// ensure main keypair is loaded
	if e.sk == "" {
		if e.prefs.PrivateKey != "" {
			e.sk = e.prefs.PrivateKey
			e.pk, _ = nostr.GetPublicKey(e.sk)
		} else {
			e.sk = nostr.GeneratePrivateKey()
			e.pk, _ = nostr.GetPublicKey(e.sk)
			e.prefs.PrivateKey = e.sk
			e.persistPrefs()
		}
	}

	identitySet := false
	if e.prefs.ActiveViewName != "" {
		e.setActiveView(e.prefs.ActiveViewName)
		identitySet = true
	} else if len(e.prefs.Views) > 0 {
		e.setActiveView(e.prefs.Views[0].Name)
		identitySet = true
	}

	if !identitySet {
		log.Println("No chat/group found on startup, generating initial ephemeral identity.")
		e.sk = nostr.GeneratePrivateKey()
		e.pk, _ = nostr.GetPublicKey(e.sk)
		if e.prefs.Nick != "" {
			e.n = e.prefs.Nick
		} else {
			e.n = npubToTokiPona(e.pk)
		}
		e.eventsChan <- SurfaceUpdate{
			Type:    "STATUS",
			Content: fmt.Sprintf("No chats joined. Initial identity: %s (%s...)", e.n, e.pk[:4]),
		}
	}

	e.sendStateUpdate(false)

	e.wg.Go(func() {
		e.updateAllSubscriptions()
		e.discoverRelays(e.prefs.AnchorRelays, 1)
	})

	for {
		select {
		case action, ok := <-e.actionsChan:
			if !ok {
				e.shutdown()
				return
			}
			e.handleAction(action)
		case <-e.ctx.Done():
			return
		}
	}
}

func (e *engine) handleAction(action InboundIntent) {
	switch action.Type {
	case "SEND_MESSAGE":
		go e.publishMessage(action.Payload)
	case "ACTIVATE_VIEW":
		e.setActiveView(action.Payload)
		e.flushAllOrdering()
	case "CREATE_GROUP":
		e.createGroup(action.Payload)
	case "JOIN_CHATS":
		e.joinChats(action.Payload)
	case "LEAVE_CHAT":
		e.leaveChat(action.Payload)
	case "DELETE_GROUP":
		e.deleteGroup(action.Payload)
	case "DELETE_VIEW":
		e.deleteView(action.Payload)
	case "REQUEST_NICK_COMPLETION":
		e.handleNickCompletion(action.Payload)
	case "SET_POW":
		e.setPoW(action.Payload)
	case "SET_NICK":
		e.setNick(action.Payload)
	case "LIST_CHATS":
		e.listChats()
	case "GET_ACTIVE_CHAT":
		e.getActiveChat()
	case "BLOCK_USER":
		e.blockUser(action.Payload)
	case "UNBLOCK_USER":
		e.unblockUser(action.Payload)
	case "LIST_BLOCKED":
		e.listBlockedUsers()
	case "HANDLE_FILTER":
		e.handleFilter(action.Payload)
	case "REMOVE_FILTER":
		e.removeFilter(action.Payload)
	case "CLEAR_FILTERS":
		e.clearFilters()
	case "HANDLE_MUTE":
		e.handleMute(action.Payload)
	case "REMOVE_MUTE":
		e.removeMute(action.Payload)
	case "CLEAR_MUTES":
		e.clearMutes()
	case "MANAGE_ANCHORS":
		e.manageAnchors(action.Payload)
	case "GET_HELP":
		e.getHelp()
	case "REQUEST_CHAT_USERS":
		e.requestParticipants(action.Payload)
	case "QUIT":
		e.shutdown()
	}
}

// manageAnchors handles adding/removing/listing anchor relays.
func (e *engine) manageAnchors(payload string) {
	args := strings.Fields(payload)

	if len(args) == 0 {
		if len(e.prefs.AnchorRelays) == 0 {
			e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: "No anchor relays set. Use /relay <url> to add one."}
			return
		}
		var builder strings.Builder
		builder.WriteString("Anchor Relays:\n")
		for i, url := range e.prefs.AnchorRelays {
			builder.WriteString(fmt.Sprintf("[%d] %s\n", i+1, url))
		}
		e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: builder.String()}
		return
	}

	if len(args) == 1 {
		idx, err := strconv.Atoi(args[0])
		if err == nil {
			if idx < 1 || idx > len(e.prefs.AnchorRelays) {
				e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Invalid index: %d. Use /relay to see the list.", idx)}
				return
			}
			removedURL := e.prefs.AnchorRelays[idx-1]
			e.prefs.AnchorRelays = append(e.prefs.AnchorRelays[:idx-1], e.prefs.AnchorRelays[idx:]...)
			e.persistPrefs()
			e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Removed anchor relay: %s", removedURL)}
			go e.updateAllSubscriptions()
			return
		}
	}

	var added []string
	var invalid []string
	existingAnchors := make(map[string]struct{}, len(e.prefs.AnchorRelays))
	for _, anchor := range e.prefs.AnchorRelays {
		existingAnchors[anchor] = struct{}{}
	}

	for _, rawURL := range args {
		url, err := normalizeRelayURL(rawURL)
		if err != nil {
			invalid = append(invalid, rawURL)
			continue
		}
		if _, exists := existingAnchors[url]; exists {
			continue
		}

		e.prefs.AnchorRelays = append(e.prefs.AnchorRelays, url)
		existingAnchors[url] = struct{}{}
		added = append(added, url)
	}

	if len(invalid) > 0 {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Invalid URL(s) skipped: %s", strings.Join(invalid, ", "))}
	}

	if len(added) > 0 {
		e.persistPrefs()
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Added anchor relay(s): %s", strings.Join(added, ", "))}
		go func() {
			e.updateAllSubscriptions()
			e.discoverRelays(added, 1)
		}()
	} else if len(invalid) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Specified relay(s) are already in the anchor list."}
	}
}

func (e *engine) shutdown() {
	e.cancel()
	e.orderMu.Lock()
	for key, t := range e.orderTimers {
		if t.Stop() {
			go e.flushOrdered(key)
		}
	}
	e.orderTimers = make(map[string]*time.Timer)
	e.orderMu.Unlock()
	e.wg.Wait()
	select {
	case e.eventsChan <- SurfaceUpdate{Type: "SHUTDOWN"}:
	case <-time.After(200 * time.Millisecond):
	}
}

// Helpers

// triggerSubUpdate safely resets a timer to call updateAllSubscriptions.
func (e *engine) triggerSubUpdate() {
	e.updateSubMu.Lock()
	defer e.updateSubMu.Unlock()

	if e.updateSubTimer != nil {
		e.updateSubTimer.Reset(debounceDelay)
		return
	}

	e.updateSubTimer = time.AfterFunc(debounceDelay, func() {
		e.updateAllSubscriptions()
		_ = e.saveDiscoveredRelayStore()

		e.updateSubMu.Lock()
		e.updateSubTimer = nil
		e.updateSubMu.Unlock()
	})
}

func (e *engine) flushAllOrdering() {
	e.orderMu.Lock()
	keys := make([]string, 0, len(e.orderTimers))
	for k := range e.orderTimers {
		keys = append(keys, k)
	}
	e.orderMu.Unlock()
	for _, k := range keys {
		e.flushOrdered(k)
	}
}

// discardOrderedStream drops buffered messages for a stream without emitting them
// (e.g. when reloading chat history after identity rotation).
func (e *engine) discardOrderedStream(streamKey string) {
	e.orderMu.Lock()
	if t, ok := e.orderTimers[streamKey]; ok {
		t.Stop()
		delete(e.orderTimers, streamKey)
	}
	delete(e.orderBuf, streamKey)
	e.orderMu.Unlock()
}

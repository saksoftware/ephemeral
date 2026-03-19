package session

import (
	"context"
	"fmt"
	"log"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmcloughlin/geohash"
	"github.com/nbd-wtf/go-nostr"
)

// Subscription & Relay Lifecycle

func (e *engine) getRelayPoolForChat(chat string) []string {
	relaySet := make(map[string]struct{})

	for _, url := range e.prefs.AnchorRelays {
		relaySet[url] = struct{}{}
	}

	if e.scoutLedger != nil {
		for _, url := range e.getDiscoveredRelayURLs() {
			relaySet[url] = struct{}{}
		}
	}

	if geohash.Validate(chat) == nil {
		closest, err := closestRelays(chat, defaultRelayCount)
		if err == nil {
			for _, url := range closest {
				relaySet[url] = struct{}{}
			}
		}
	}

	relayURLs := make([]string, 0, len(relaySet))
	for url := range relaySet {
		relayURLs = append(relayURLs, url)
	}

	if len(relayURLs) == 0 {
		relayURLs = defaultEphChatRelays
	}

	return relayURLs
}

func (e *engine) updateAllSubscriptions() {
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
		e.updateRelaySubscriptions(make(map[string][]string))
		e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "No active chat — relays idle."}
		return
	}

	desiredRelayToChats := make(map[string][]string)
	for chat := range activeChats {
		relayURLs := e.getRelayPoolForChat(chat)
		for _, url := range relayURLs {
			found := slices.Contains(desiredRelayToChats[url], chat)
			if !found {
				desiredRelayToChats[url] = append(desiredRelayToChats[url], chat)
			}
		}
	}

	e.updateRelaySubscriptions(desiredRelayToChats)

	if activeView != nil && len(desiredRelayToChats) > 0 {
		e.emitRelayListenStatus(activeView, len(desiredRelayToChats), false)
	}
}

func (e *engine) updateRelaySubscriptions(desiredRelays map[string][]string) {
	e.updateRelaySubscriptionsWithRefresh(desiredRelays, false)
}

func (e *engine) updateRelaySubscriptionsWithRefresh(desiredRelays map[string][]string, forceRefresh bool) {
	want := make([]string, 0, len(desiredRelays))
	for u := range desiredRelays {
		want = append(want, u)
	}
	slices.Sort(want)
	e.relaysMu.Lock()
	e.lastSubscriptionRelayURLs = want
	e.relaysMu.Unlock()

	e.relaysMu.Lock()
	currentRelays := make(map[string]*relayConn, len(e.relays))
	maps.Copy(currentRelays, e.relays)
	e.relaysMu.Unlock()

	var wg sync.WaitGroup
	for url, chats := range desiredRelays {

		if e.relayFailed(url) {
			continue
		}

		if mr, exists := currentRelays[url]; exists {
			wg.Add(1)
			go func(mr *relayConn, chats []string) {
				defer wg.Done()
				if _, err := e.replaceSubscriptionWithRefresh(mr, chats, forceRefresh); err != nil {
					e.eventsChan <- SurfaceUpdate{
						Type:    "ERROR",
						Content: fmt.Sprintf("Resubscribe failed on %s: %v", mr.url, err),
					}

					e.markRelayFailed(url)
				}
			}(mr, chats)
		} else {
			go e.manageRelayConnection(url, chats)
		}
	}

	e.relaysMu.Lock()
	for url, mr := range e.relays {
		if _, needed := desiredRelays[url]; !needed {
			log.Printf("Disconnecting from unneeded relay: %s", url)
			mr.mu.Lock()
			if mr.subscription != nil {
				mr.subscription.Unsub()
				mr.subscription = nil
			}
			if mr.relay != nil {
				mr.relay.Close()
			}
			mr.mu.Unlock()
			delete(e.relays, url)
		}
	}
	e.relaysMu.Unlock()

	wg.Wait()
	e.sendRelaysUpdate()
}

func (e *engine) manageRelayConnection(url string, chats []string) {
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()

	if e.relayFailed(url) {
		e.eventsChan <- SurfaceUpdate{
			Type:    "STATUS",
			Content: fmt.Sprintf("Skipping connect to discovered relay %s (in fail cache)", url),
		}
		return
	}

	start := time.Now()
	relay, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		if !e.isDiscoveredRelay(url) {
			e.eventsChan <- SurfaceUpdate{
				Type:    "ERROR",
				Content: fmt.Sprintf("Failed to connect to %s: %v", url, err),
			}
		}

		e.markRelayFailed(url)
		return
	}
	latency := time.Since(start)

	mr := &relayConn{
		url:               url,
		relay:             relay,
		latency:           latency,
		connected:         true,
		reconnectAttempts: 0,
	}

	e.relaysMu.Lock()
	if _, exists := e.relays[url]; exists {
		e.relaysMu.Unlock()
		relay.Close()
		return
	}
	e.relays[url] = mr
	e.relaysMu.Unlock()
	e.sendRelaysUpdate()

	if _, err := e.replaceSubscription(mr, chats); err != nil {
		if e.isDiscoveredRelay(mr.url) && e.verifyFailCache != nil {
			e.markRelayFailed(mr.url)
		}

		relay.Close()
		e.relaysMu.Lock()
		delete(e.relays, mr.url)
		e.relaysMu.Unlock()
		e.sendRelaysUpdate()
		return
	}

	e.wg.Go(func() {
		e.listenForEvents(mr)
	})
}

func (e *engine) replaceSubscription(mr *relayConn, chats []string) (bool, error) {
	return e.replaceSubscriptionWithRefresh(mr, chats, false)
}

func (e *engine) replaceSubscriptionWithRefresh(mr *relayConn, chats []string, forceRefresh bool) (bool, error) {
	mr.mu.Lock()
	oldChats := mrCurrentChatsLocked(mr.subscription)
	mr.mu.Unlock()

	if !forceRefresh && sameStringSet(oldChats, chats) {
		return false, nil
	}

	now := nostr.Now()
	lookbackSeconds := e.historyLookbackSeconds()
	filters := make(nostr.Filters, 0, len(chats))
	for _, ch := range chats {
		// Request a window of past events so history is visible immediately
		// on joining/switching chats.
		since := now - lookbackSeconds
		if geohash.Validate(ch) == nil {
			filters = append(filters, nostr.Filter{
				Kinds: []int{geoChatKind},
				Tags:  nostr.TagMap{"g": []string{ch}},
				Since: &since,
				Limit: messageHistoryLimit,
			})
		} else {
			filters = append(filters, nostr.Filter{
				Kinds: []int{ephChatKind},
				Tags:  nostr.TagMap{"d": []string{ch}},
				Since: &since,
				Limit: messageHistoryLimit,
			})
		}
	}

	newSub, err := mr.relay.Subscribe(e.ctx, filters)
	if err != nil {
		// Some relay disconnects keep the subscription channel closed and
		// make Subscribe fail with "not connected". Reconnect and retry.
		if mr.relay != nil {
			_ = mr.relay.Close()
		}

		connectCtx, cancel := context.WithTimeout(e.ctx, connectTimeout)
		start := time.Now()
		relay, connErr := nostr.RelayConnect(connectCtx, mr.url)
		cancel()
		if connErr != nil {
			return false, fmt.Errorf("subscribe failed: %w (reconnect failed: %v)", err, connErr)
		}

		mr.mu.Lock()
		mr.relay = relay
		mr.latency = time.Since(start)
		mr.connected = true
		mr.reconnectAttempts = 0
		mr.mu.Unlock()

		newSub, err = relay.Subscribe(e.ctx, filters)
		if err != nil {
			return false, fmt.Errorf("subscribe failed after reconnect: %w", err)
		}
	}

	mr.mu.Lock()
	oldSub := mr.subscription
	mr.subscription = newSub
	mr.mu.Unlock()

	if oldSub != nil {
		oldSub.Unsub()
	}

	e.sendRelaysUpdate()

	return true, nil
}

func (e *engine) sendRelaysUpdate() {
	e.relaysMu.Lock()

	desired := append([]string(nil), e.lastSubscriptionRelayURLs...)
	// Include every desired URL in the list: from e.relays or as down (not yet in e.relays)
	statuses := make([]RelayEndpoint, 0, len(desired)+len(e.relays))
	seen := make(map[string]bool, len(e.relays))
	for _, mr := range e.relays {
		mr.mu.Lock()
		connected := mr.connected
		latency := mr.latency
		mr.mu.Unlock()
		seen[mr.url] = true
		statuses = append(statuses, RelayEndpoint{
			URL:       mr.url,
			Latency:   latency,
			Connected: connected,
		})
	}
	for _, url := range desired {
		if seen[url] {
			continue
		}
		// Desired but not in e.relays (connect failed or pending) — show as down with ✗
		statuses = append(statuses, RelayEndpoint{
			URL:       url,
			Latency:   0,
			Connected: false,
		})
	}

	up := 0
	for _, url := range desired {
		if e.relayFailed(url) {
			continue
		}
		mr, ok := e.relays[url]
		if !ok {
			continue
		}
		mr.mu.Lock()
		conn := mr.connected
		mr.mu.Unlock()
		if conn {
			up++
		}
	}
	down := len(desired) - up

	e.relaysMu.Unlock()

	e.eventsChan <- SurfaceUpdate{Type: "RELAYS_UPDATE", Payload: RelayPanelSnapshot{
		Relays:    statuses,
		UpCount:   up,
		DownCount: down,
	}}
}

// Event Ingestion & Processing

func (e *engine) listenForEvents(mr *relayConn) {
	const maxReconnectAttempts = 3

	for {
		if e.ctx.Err() != nil {
			return
		}

		mr.mu.Lock()
		sub := mr.subscription
		mr.mu.Unlock()

		if sub == nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		select {
		case <-e.ctx.Done():
			return

		case ev, ok := <-sub.Events:
			if !ok {
				oldChats := mrCurrentChatsLocked(sub)

				mr.mu.Lock()
				if mr.subscription != sub {
					mr.mu.Unlock()
					continue
				}
				mr.subscription = nil
				mr.connected = false
				mr.mu.Unlock()

				e.sendRelaysUpdate()

				if e.isDiscoveredRelay(mr.url) {
					e.relaysMu.Lock()
					delete(e.relays, mr.url)
					e.relaysMu.Unlock()

					e.markRelayFailed(mr.url)

					e.sendRelaysUpdate()
					return
				}

				if len(oldChats) == 0 {
					e.relaysMu.Lock()
					delete(e.relays, mr.url)
					e.relaysMu.Unlock()
					e.sendRelaysUpdate()
					return
				}

				mr.mu.Lock()
				mr.reconnectAttempts++
				attempts := mr.reconnectAttempts
				mr.mu.Unlock()

				if attempts > maxReconnectAttempts {
					e.eventsChan <- SurfaceUpdate{
						Type:    "ERROR",
						Content: fmt.Sprintf("Anchor/Geo relay %s failed to reconnect after %d attempts. Giving up.", mr.url, maxReconnectAttempts),
					}

					e.relaysMu.Lock()
					delete(e.relays, mr.url)
					e.relaysMu.Unlock()
					e.sendRelaysUpdate()
					return
				}

				err := retryWithBackoff(e.ctx, func() error {
					_, err := e.replaceSubscription(mr, oldChats)
					return err
				}, attempts)

				if err != nil {
					e.eventsChan <- SurfaceUpdate{
						Type:    "ERROR",
						Content: fmt.Sprintf("Could not re-establish subscription on %s (attempt %d). Error: %v. Listener stopped.", mr.url, attempts, err),
					}
					e.relaysMu.Lock()
					delete(e.relays, mr.url)
					e.relaysMu.Unlock()
					e.sendRelaysUpdate()
					return
				}

				mr.mu.Lock()
				mr.connected = true
				mr.reconnectAttempts = 0
				mr.mu.Unlock()
				e.sendRelaysUpdate()
				continue
			}

			if ev == nil {
				continue
			}
			e.processEvent(ev, mr.url)
		}
	}
}

func (e *engine) processEvent(ev *nostr.Event, relayURL string) {
	for _, blockedPeer := range e.prefs.BlockedUsers {
		if ev.PubKey == blockedPeer.PubKey {
			return
		}
	}

	e.seenCacheMu.Lock()
	if e.seenCache.Contains(ev.ID) {
		e.seenCacheMu.Unlock()
		return
	}
	e.seenCache.Add(ev.ID, true)
	e.seenCacheMu.Unlock()

	var eventChat string
	if gTag := ev.Tags.Find("g"); len(gTag) > 1 {
		eventChat = gTag[1]
	} else if dTag := ev.Tags.Find("d"); len(dTag) > 1 {
		eventChat = dTag[1]
	}

	if eventChat == "" {
		return
	}

	activeView := e.getActiveView()
	if activeView != nil {
		isRelevantToActiveView := false
		if activeView.IsGroup {
			if slices.Contains(activeView.Children, eventChat) {
				isRelevantToActiveView = true
			}
		} else {
			if activeView.Name == eventChat {
				isRelevantToActiveView = true
			}
		}

		if isRelevantToActiveView {
			requiredPoW := e.effectivePoWForChat(eventChat)
			if !isPoWValid(ev, requiredPoW) {
				log.Printf("Dropped event %s from %s for failing PoW check (required: %d)", safeSuffix(ev.ID, 4), eventChat, requiredPoW)
				return
			}
		}
	}

	streamKey := "chat:" + eventChat
	if av := e.getActiveView(); av != nil && av.IsGroup && slices.Contains(av.Children, eventChat) {
		streamKey = "group:" + av.Name
	}

	content := truncateString(ev.Content, MaxMsgLen)
	content = sanitizeString(content)

	if e.matchesAny(content, e.mutesCompiled) {
		return
	}
	if len(e.filtersCompiled) > 0 && !e.matchesAny(content, e.filtersCompiled) {
		return
	}

	nick := npubToTokiPona(ev.PubKey)
	spk := ev.PubKey[:4]
	if nickTag := ev.Tags.Find("n"); len(nickTag) > 1 {
		if s := sanitizeString(nickTag[1]); s != "" {
			nick = s
		}
		spk = safeSuffix(ev.PubKey, 4)
	}

	prevCtx, hadPrev := e.identityCache.Get(ev.PubKey)

	// If this user wasn't known before (or context changed), notify UI immediately.
	// This is what makes the Users list update in real-time.
	shouldNotifyUI := !hadPrev ||
		prevCtx.chat != eventChat ||
		prevCtx.nick != nick ||
		prevCtx.shortPubKey != spk

	e.identityCache.Add(ev.PubKey, participantRow{
		nick:        nick,
		chat:        eventChat,
		shortPubKey: spk,
		lastMsgAt:   int64(ev.CreatedAt),
	})

	if shouldNotifyUI {
		e.eventsChan <- SurfaceUpdate{
			Type: "CHAT_USER_DISCOVERED",
			Payload: Participant{
				PubKey:      ev.PubKey,
				Nick:        nick,
				ShortPubKey: spk,
				Chat:        eventChat,
				LastMsgAt:   int64(ev.CreatedAt),
			},
		}
	}

	timestamp := time.Unix(int64(ev.CreatedAt), 0).Format("15:04:05")

	isOwn := false

	if ev.PubKey == e.pk {
		isOwn = true
	} else {
		for _, s := range e.chatKeys {
			if ev.PubKey == s.pubKey {
				isOwn = true
				break
			}
		}
	}

	e.enqueueOrdered(streamKey, SurfaceUpdate{
		Type:         "NEW_MESSAGE",
		Timestamp:    timestamp,
		CreatedAt:    int64(ev.CreatedAt),
		Nick:         nick,
		FullPubKey:   ev.PubKey,
		ShortPubKey:  spk,
		IsOwnMessage: isOwn,
		Content:      content,
		ID:           safeSuffix(ev.ID, 4),
		Chat:         eventChat,
		RelayURL:     relayURL,
	}, int64(ev.CreatedAt), ev.ID)
}

func (e *engine) enqueueOrdered(streamKey string, de SurfaceUpdate, createdAt int64, id string) {
	e.orderMu.Lock()
	if len(e.orderBuf[streamKey]) >= perStreamBufferMax {
		e.orderBuf[streamKey] = e.orderBuf[streamKey][1:]
	}
	e.orderBuf[streamKey] = append(e.orderBuf[streamKey], queuedFrame{ev: de, createdAt: createdAt, id: id})
	if _, ok := e.orderTimers[streamKey]; !ok {
		e.orderTimers[streamKey] = time.AfterFunc(orderingFlushDelay, func() { e.flushOrdered(streamKey) })
	}
	e.orderMu.Unlock()
}

func (e *engine) flushOrdered(streamKey string) {
	e.orderMu.Lock()
	buf := e.orderBuf[streamKey]
	delete(e.orderBuf, streamKey)
	delete(e.orderTimers, streamKey)
	e.orderMu.Unlock()

	if len(buf) == 0 {
		return
	}

	sort.Slice(buf, func(i, j int) bool {
		if buf[i].createdAt == buf[j].createdAt {
			return buf[i].id < buf[j].id
		}
		return buf[i].createdAt < buf[j].createdAt
	})

	for _, it := range buf {
		select {
		case e.eventsChan <- it.ev:
		case <-e.ctx.Done():
			return
		}
	}
}

// Message Publishing Lifecycle

func (e *engine) publishMessage(message string) {
	var targetChat string
	var targetPubKey string
	if strings.HasPrefix(message, "@") {
		var matchedReplyTag string
		for _, pk := range e.identityCache.Keys() {
			if ctx, ok := e.identityCache.Get(pk); ok {
				replyTag := fmt.Sprintf("@%s#%s", ctx.nick, ctx.shortPubKey)
				if strings.HasPrefix(message, replyTag) {
					if len(replyTag) > len(matchedReplyTag) {
						matchedReplyTag = replyTag
						targetPubKey = pk
						targetChat = ctx.chat
					}
				}
			}
		}

		if targetPubKey == "" {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Could not find a known user matching your message prefix."}
			return
		}
	} else {
		activeView := e.getActiveView()
		if activeView == nil {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "No active chat/group to send message to."}
			return
		}
		if activeView.IsGroup {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Broadcasting to a group is disabled. Use @nick to send a message."}
			return
		}
		if activeView.Name == "" {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "The active chat is invalid."}
			return
		}
		targetChat = activeView.Name
	}

	var kind int
	var tagKey string

	if geohash.Validate(targetChat) == nil {
		kind = geoChatKind
		tagKey = "g"
	} else {
		kind = ephChatKind
		tagKey = "d"
	}

	relayPool := e.getRelayPoolForChat(targetChat)
	relayPoolSet := make(map[string]struct{}, len(relayPool))
	for _, url := range relayPool {
		relayPoolSet[url] = struct{}{}
	}

	tags := nostr.Tags{{tagKey, targetChat}}
	if targetPubKey != "" {
		tags = append(tags, nostr.Tag{"p", targetPubKey})
	}

	activeView := e.getActiveView()
	if activeView == nil {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Cannot determine PoW: No active chat/group."}
		return
	}
	requiredPoW := e.effectivePoWForChat(targetChat)

	e.relaysMu.Lock()
	var relaysForPublishing []*relayConn
	for url, r := range e.relays {
		if _, ok := relayPoolSet[url]; !ok {
			continue
		}
		if e.relayFailed(url) {
			continue
		}
		relaysForPublishing = append(relaysForPublishing, r)
	}
	e.relaysMu.Unlock()

	if len(relaysForPublishing) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Not connected to any suitable relays for chat %s", targetChat)}
		return
	}

	ev := e.createEvent(message, kind, tags, requiredPoW)

	if requiredPoW > 0 {
		go e.minePoWAndPublish(ev, requiredPoW, targetChat, relaysForPublishing)
	} else {
		if err := e.signEventForChat(&ev, targetChat); err != nil {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Failed to sign event: %v", err)}
			return
		}
		e.publish(ev, targetChat, relaysForPublishing)
	}
}

func (e *engine) createEvent(message string, kind int, tags nostr.Tags, difficulty int) nostr.Event {
	baseTags := make(nostr.Tags, 0, len(tags)+2)
	baseTags = append(baseTags, tags...)

	active := e.getActiveView()
	if active != nil && !active.IsGroup {
		if session, ok := e.chatKeys[active.Name]; ok && session.nick != "" {
			baseTags = append(baseTags, nostr.Tag{"n", session.nick})
		}
	} else if active != nil && active.IsGroup {
		nick := e.prefs.Nick
		if nick == "" {
			nick = npubToTokiPona(e.pk)
		}
		baseTags = append(baseTags, nostr.Tag{"n", nick})
	}

	ev := nostr.Event{
		CreatedAt: nostr.Now(),
		PubKey:    e.pk,
		Content:   message,
		Kind:      kind,
		Tags:      baseTags,
	}

	if difficulty > 0 {
		ev.Tags = append(ev.Tags, nostr.Tag{"nonce", "0", strconv.Itoa(difficulty)})
	}

	return ev
}

func (e *engine) signEventForChat(ev *nostr.Event, chatName string) error {
	view := e.getActiveView()
	useMainKey := false

	if view != nil && view.IsGroup {
		useMainKey = true
	}

	if !useMainKey {
		if session, ok := e.chatKeys[chatName]; ok && session.privKey != "" {
			ev.PubKey = session.pubKey
			ev.ID = ev.GetID()
			return ev.Sign(session.privKey)
		}
	}

	if e.sk == "" || e.pk == "" {
		return fmt.Errorf("no valid signing key available")
	}

	ev.PubKey = e.pk
	ev.ID = ev.GetID()
	return ev.Sign(e.sk)
}

func (e *engine) minePoWAndPublish(ev nostr.Event, difficulty int, targetChat string, relays []*relayConn) {
	if session, ok := e.chatKeys[targetChat]; ok && session.privKey != "" {
		ev.PubKey = session.pubKey
	} else {
		ev.PubKey = e.pk
	}

	e.eventsChan <- SurfaceUpdate{Type: "STATUS",
		Content: fmt.Sprintf("Calculating Proof-of-Work (difficulty %d)...", difficulty),
	}

	nonceTagIndex := -1
	for i, tag := range ev.Tags {
		if len(tag) > 1 && tag[0] == "nonce" {
			nonceTagIndex = i
			break
		}
	}
	if nonceTagIndex == -1 {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "PoW mining failed: nonce tag not found."}
		return
	}

	var nonceCounter uint64
	for {
		ev.Tags[nonceTagIndex][1] = strconv.FormatUint(nonceCounter, 10)
		ev.ID = ev.GetID()
		if countLeadingZeroBits(ev.ID) >= difficulty {
			break
		}
		nonceCounter++
		if nonceCounter&0x3FF == 0 {
			select {
			case <-e.ctx.Done():
				e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "PoW calculation cancelled."}
				return
			default:
			}
		}
	}

	if session, ok := e.chatKeys[targetChat]; ok && session.privKey != "" {
		_ = ev.Sign(session.privKey)
	} else {
		_ = ev.Sign(e.sk)
	}

	e.publish(ev, targetChat, relays)
}

func (e *engine) publish(ev nostr.Event, targetChat string, relaysForPublishing []*relayConn) {
	sort.Slice(relaysForPublishing, func(i, j int) bool {
		return relaysForPublishing[i].latency < relaysForPublishing[j].latency
	})

	var wg sync.WaitGroup
	successCount := 0
	var errorMessages []string
	var mu sync.Mutex

	for _, r := range relaysForPublishing {
		wg.Add(1)
		go func(r *relayConn) {
			defer wg.Done()
			if err := r.relay.Publish(e.ctx, ev); err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			} else {
				mu.Lock()
				errorMessages = append(errorMessages, fmt.Sprintf("%s: %v", r.url, err))
				mu.Unlock()

				r.mu.Lock()
				r.connected = false
				r.mu.Unlock()

				e.markRelayFailed(r.url)

				e.sendRelaysUpdate()
			}
		}(r)
	}
	wg.Wait()

	e.eventsChan <- SurfaceUpdate{
		Type: "STATUS",
		Content: fmt.Sprintf("Event %s sent to %d/%d relays for %s.",
			safeSuffix(ev.ID, 4), successCount, len(relaysForPublishing), targetChat),
	}

	for _, em := range errorMessages {
		e.eventsChan <- SurfaceUpdate{
			Type: "ERROR", Content: "Publish failed on " + em,
		}
		if pow, ok := parsePowHint(em); ok && pow > 0 {
			e.eventsChan <- SurfaceUpdate{
				Type:    "INFO",
				Content: fmt.Sprintf("Hint: relay suggests PoW %d for %s. Try `/pow %d` and resend.", pow, targetChat, pow),
			}
		}
	}
}

// Helpers

func (e *engine) effectivePoWForChat(chat string) int {
	for _, v := range e.prefs.Views {
		if !v.IsGroup && v.Name == chat && v.PoW > 0 {
			return v.PoW
		}
	}
	if av := e.getActiveView(); av != nil && av.IsGroup && av.PoW > 0 {
		if slices.Contains(av.Children, chat) {
			return av.PoW
		}
	}
	return 0
}

func retryWithBackoff(ctx context.Context, fn func() error, attempt int) error {
	delay := min(time.Duration(math.Pow(2, float64(attempt-1)))*500*time.Millisecond, 30*time.Second)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		if err := fn(); err != nil {
			return err
		}
		return nil
	}
}

func mrCurrentChatsLocked(sub *nostr.Subscription) []string {
	if sub == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, f := range sub.Filters {
		tagsToCheck := [][]string{}
		if gTags, ok := f.Tags["g"]; ok {
			tagsToCheck = append(tagsToCheck, gTags)
		}
		if dTags, ok := f.Tags["d"]; ok {
			tagsToCheck = append(tagsToCheck, dTags)
		}

		for _, tagSet := range tagsToCheck {
			for _, ch := range tagSet {
				if _, exists := seen[ch]; !exists {
					seen[ch] = struct{}{}
					out = append(out, ch)
				}
			}
		}
	}
	return out
}

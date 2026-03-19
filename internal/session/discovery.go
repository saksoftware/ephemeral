package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

const (
	maxDiscoveryDepth    = 2
	maxActiveDiscoveries = 10
	discoveryKind        = 10002
	connectTimeout       = 10 * time.Second
	verifyTimeout        = 5 * time.Second
	debounceDelay        = 60 * time.Second
)

// ScoutRelayEntry describes a relay entry in relays.json.
type ScoutRelayEntry struct {
	URL      string `json:"url"`
	LastSeen int64  `json:"last_seen"`
}

type relayLedger struct {
	mu     sync.RWMutex
	Path   string
	Relays map[string]ScoutRelayEntry
}

// Persistent store management

func (e *engine) loadDiscoveredRelayStore() error {
	appConfigDir, err := getAppConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(appConfigDir, "relays.json")

	s := &relayLedger{Path: path, Relays: make(map[string]ScoutRelayEntry)}
	data, err := os.ReadFile(path)
	if err == nil {
		var tmp struct {
			Discovered []ScoutRelayEntry `json:"discovered"`
		}
		if json.Unmarshal(data, &tmp) == nil {
			for _, r := range tmp.Discovered {
				s.Relays[r.URL] = r
			}
		}
	}

	e.scoutLedger = s
	return nil
}

func (e *engine) saveDiscoveredRelayStore() error {
	s := e.scoutLedger
	s.mu.RLock()
	list := make([]ScoutRelayEntry, 0, len(s.Relays))
	for _, r := range s.Relays {
		list = append(list, r)
	}
	s.mu.RUnlock()

	data, _ := json.MarshalIndent(map[string]any{"discovered": list}, "", "  ")
	tmpPath := s.Path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.Path)
}

func (e *engine) getDiscoveredRelayURLs() []string {
	e.scoutLedger.mu.RLock()
	defer e.scoutLedger.mu.RUnlock()

	urls := make([]string, 0, len(e.scoutLedger.Relays))
	for url := range e.scoutLedger.Relays {
		urls = append(urls, url)
	}
	return urls
}

// Discovery logic

func (e *engine) discoverRelays(anchors []string, depth int) {
	for _, anchor := range anchors {
		norm, err := normalizeRelayURL(anchor)
		if err != nil {
			continue
		}
		e.wg.Add(1)
		go e.discoverOnAnchor(norm, depth)
	}
}

// discoverOnAnchor connects to an anchor relay and listens for kind=10002,
// automatically reconnecting on failure. Event processing is asynchronous
// to avoid blocking the subscription feed.
func (e *engine) discoverOnAnchor(anchorURL string, depth int) {
	defer e.wg.Done()

	if depth > maxDiscoveryDepth {
		return
	}

	if atomic.LoadInt32(&e.activeDiscoveries) >= maxActiveDiscoveries {
		return
	}
	atomic.AddInt32(&e.activeDiscoveries, 1)
	defer atomic.AddInt32(&e.activeDiscoveries, -1)

	for {
		// If client is shutting down, exit
		select {
		case <-e.ctx.Done():
			return
		default:
		}

		// connection with a short timeout
		connectCtx, cancelConnect := context.WithTimeout(e.ctx, connectTimeout)
		relay, err := nostr.RelayConnect(connectCtx, anchorURL)
		cancelConnect()
		if err != nil {
			time.Sleep(15 * time.Second) // wait before reconnecting
			continue
		}

		// subscription to 10002
		f := nostr.Filter{Kinds: []int{discoveryKind}}
		sub, err := relay.Subscribe(e.ctx, nostr.Filters{f})
		if err != nil {
			relay.Close()
			time.Sleep(15 * time.Second) // wait before reconnecting
			continue
		}

		// event reading loop
		for {
			select {
			case <-e.ctx.Done():
				sub.Unsub()
				relay.Close()
				return

			case ev, ok := <-sub.Events:
				if !ok {
					// connection lost - trigger reconnect
					sub.Unsub()
					relay.Close()
					time.Sleep(5 * time.Second)
					goto retry // break inner loop, continue outer
				}

				// Process async to avoid blocking the event feed
				e.wg.Add(1)
				go func(evt *nostr.Event) {
					defer e.wg.Done()
					e.parseRelayEvent(evt, verifyTimeout, depth)
				}(ev)
			}
		}

	retry:
		continue
	}
}

// parseRelayEvent processes a kind=10002 event and asynchronously verifies
// new relays. Verification is done in separate goroutines.
func (e *engine) parseRelayEvent(ev *nostr.Event, verifyTimeout time.Duration, depth int) {
	if ev.Kind != discoveryKind {
		return
	}

	store := e.scoutLedger

	for _, tag := range ev.Tags {
		if len(tag) < 2 || tag[0] != "r" {
			continue
		}

		url, err := normalizeRelayURL(tag[1])
		if err != nil {
			continue
		}

		// skip read/write specific
		if len(tag) >= 3 {
			mode := strings.ToLower(strings.TrimSpace(tag[2]))
			if mode == "read" || mode == "write" {
				continue
			}
		}

		// skip if it's one of our own anchor relays
		isAnchor := false
		for _, a := range e.prefs.AnchorRelays {
			na, err := normalizeRelayURL(a)
			if err == nil && na == url {
				isAnchor = true
				break
			}
		}
		if isAnchor {
			continue
		}

		// if in fail-cache, skip
		if e.verifyFailCache != nil && e.verifyFailCache.Contains(url) {
			continue
		}

		// uniqueness check block
		e.verifyingMu.Lock()

		// already being verified
		if _, ok := e.verifying[url]; ok {
			e.verifyingMu.Unlock()
			continue
		}

		// already active
		if _, ok := e.relays[url]; ok {
			e.verifyingMu.Unlock()
			continue
		}

		// already in discovered
		if _, ok := store.Relays[url]; ok {
			e.verifyingMu.Unlock()
			continue
		}

		// mark as "being verified"
		e.verifying[url] = struct{}{}
		e.verifyingMu.Unlock()

		// async verification
		e.wg.Add(1)
		go func(url string) {
			defer e.wg.Done()

			// remove from "verifying" map when done
			defer func() {
				e.verifyingMu.Lock()
				delete(e.verifying, url)
				e.verifyingMu.Unlock()
			}()

			ok := e.verifyRelay(url, verifyTimeout)
			if !ok {
				// add to fail-cache
				if e.verifyFailCache != nil {
					e.verifyFailCache.Add(url, true)
				}
				return
			}

			// save to scoutLedger
			store.mu.Lock()
			store.Relays[url] = ScoutRelayEntry{
				URL:      url,
				LastSeen: time.Now().Unix(),
			}
			store.mu.Unlock()

			// connect immediately
			go e.manageRelayConnection(url, nil)

			// update subscriptions (debounced)
			e.triggerSubUpdate()

			// recursive discovery (if depth allows)
			if depth < maxDiscoveryDepth {
				e.wg.Add(1)
				go e.discoverOnAnchor(url, depth+1)
			}

		}(url)
	}
}

// Verification logic

func (e *engine) verifyRelay(url string, timeout time.Duration) bool {
	rctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()

	relay, err := nostr.RelayConnect(rctx, url)
	if err != nil {
		return false
	}
	defer relay.Close()

	// create test event
	dummy := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      geoChatKind, // Kind=20000
		Tags:      nostr.Tags{{"client", "ephemeral"}},
		Content:   "",
		PubKey:    e.pk,
	}
	if e.sk == "" {
		return false
	}
	if err := dummy.Sign(e.sk); err != nil {
		return false
	}

	// try to publish
	if err := relay.Publish(rctx, dummy); err != nil {
		return false // publish failed
	}

	// now read this event back by its ID
	readCtx, cancelRead := context.WithTimeout(rctx, timeout/2)
	defer cancelRead()

	f := nostr.Filter{
		Kinds: []int{geoChatKind},
		IDs:   []string{dummy.ID},
		Limit: 1,
	}

	sub, err := relay.Subscribe(readCtx, nostr.Filters{f})
	if err != nil {
		return false
	}
	defer sub.Unsub()

	gotResponse := false
	for {
		select {
		case <-readCtx.Done():
			return gotResponse

		case ev, ok := <-sub.Events:
			if !ok {
				return false
			}
			if ev != nil {
				gotResponse = true
			}

		case <-sub.EndOfStoredEvents:
			return true
		}
	}
}

// Helpers

func (e *engine) isDiscoveredRelay(url string) bool {
	if e.scoutLedger == nil {
		return false
	}
	e.scoutLedger.mu.RLock()
	_, ok := e.scoutLedger.Relays[url]
	e.scoutLedger.mu.RUnlock()
	return ok
}

// relayFailed checks if a discovered relay is in the fail cache.
func (e *engine) relayFailed(url string) bool {
	if e.verifyFailCache == nil || !e.isDiscoveredRelay(url) {
		return false
	}
	norm, err := normalizeRelayURL(url)
	if err != nil {
		return false
	}
	return e.verifyFailCache.Contains(norm)
}

// markRelayFailed adds a discovered relay to the fail cache.
func (e *engine) markRelayFailed(url string) {
	if e.verifyFailCache == nil || !e.isDiscoveredRelay(url) {
		return
	}
	norm, err := normalizeRelayURL(url)
	if err != nil {
		return
	}
	e.verifyFailCache.Add(norm, true)
}

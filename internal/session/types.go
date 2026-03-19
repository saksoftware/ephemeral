package session

import (
	"regexp"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// Engine-wide constants.
const (
	defaultRelayCount     = 5
	geoChatKind           = 20000
	ephChatKind           = 23333
	seenCacheSize         = 8192
	identityCacheCapacity = 4096
	MaxMsgLen             = 2000
	maxChatNameLen        = 12
	orderingFlushDelay    = 200 * time.Millisecond
	perStreamBufferMax    = 256
	defaultHistoryMin     = 10
	maxHistoryMin         = 24 * 60
	messageHistoryLimit   = 2000
)

var defaultEphChatRelays = []string{
	"wss://relay.damus.io",
	"wss://relay.primal.net",
	"wss://offchain.pub",
	"wss://adre.su",
}

// InboundIntent is a command from the terminal toward the session engine.
type InboundIntent struct {
	Type    string
	Payload string
}

// RelayEndpoint describes one relay row for the UI panel.
type RelayEndpoint struct {
	URL       string
	Latency   time.Duration
	Connected bool
}

// RelayPanelSnapshot accompanies RELAYS_UPDATE events.
type RelayPanelSnapshot struct {
	Relays    []RelayEndpoint
	UpCount   int
	DownCount int
}

// SurfaceUpdate is emitted from the engine to the terminal layer.
type SurfaceUpdate struct {
	Type         string
	Timestamp    string
	CreatedAt    int64
	Nick         string
	Content      string
	FullPubKey   string
	ShortPubKey  string
	IsOwnMessage bool
	RelayURL     string
	ID           string
	Chat         string
	Payload      any
}

// Participant is a cached roster entry for a pubkey in a room.
type Participant struct {
	PubKey      string
	Nick        string
	ShortPubKey string
	Chat        string
	LastMsgAt   int64
}

type queuedFrame struct {
	ev        SurfaceUpdate
	createdAt int64
	id        string
}

// LayoutSnapshot is the STATE_UPDATE payload for chrome synchronization.
type LayoutSnapshot struct {
	Views            []RoomSpec
	ActiveViewIndex  int
	Nick             string
	ShortPubKey      string
	ClearMessagePane bool
}

type roomIdentity struct {
	privKey    string
	pubKey     string
	nick       string
	customNick bool
}

// participantRow is LRU value: last-seen nick/hash for a pubkey.
type participantRow struct {
	nick        string
	chat        string
	shortPubKey string
	lastMsgAt   int64
}

// relayConn augments a nostr.Relay with subscription bookkeeping.
type relayConn struct {
	url               string
	relay             *nostr.Relay
	latency           time.Duration
	subscription      *nostr.Subscription
	connected         bool
	reconnectAttempts int
	mu                sync.Mutex
}

// matchRule is a literal or compiled regex for filters/mutes.
type matchRule struct {
	raw     string
	regex   *regexp.Regexp
	literal string
}

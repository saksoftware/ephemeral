package terminal

import (
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lessucettes/ephemeral/internal/session"
)

// console owns the tview primitives and bridges user input to the session engine.
type console struct {
	app         *tview.Application
	actionsChan chan<- session.InboundIntent

	// UI Components

	mainFlex            *tview.Flex
	chatList            *tview.List
	userList            *tview.List
	detailsView         *tview.List
	relaysFooter        *tview.TextView
	relaysPanel         *tview.Flex
	logs                *tview.TextView
	logsMaxList         *tview.List // fullscreen logs: one row per line, visible selection
	maximizedLogsFlex   *tview.Flex
	output              *tview.List
	maximizedOutputFlex *tview.Flex
	input               *tview.InputField
	hints               *tview.TextView
	wideComposerFlex    *tview.Flex
	wideBottomFlex      *tview.Flex
	narrowComposerFlex  *tview.Flex

	// UI State

	logsMaximized   bool
	outputMaximized bool
	narrowMode      bool
	theme           *theme

	narrowFlex  *tview.Flex
	contentGrid *tview.Grid // wide layout middle (messages + sidebar)

	// Current user's identity short prefix (for the active view).
	selfShortPubKey string

	// App Data

	views            []session.RoomSpec
	relays           []session.RelayEndpoint
	relaysUpCount    int
	relaysDownCount  int
	selectedForGroup map[string]bool
	activeViewIndex  int
	nick             string

	chatUsers         []session.Participant
	chatUsersByPubKey map[string]session.Participant

	chatListItems []chatListItem

	outputMessages      []outputMessage
	outputRowToMsg      []int // list row index -> outputMessages index
	messagesCachedWrapW int

	userPruneStopCh chan struct{}

	// Resize/layout switching (debounced).
	resizeMu          sync.Mutex
	desiredNarrowMode bool
	resizeApplyTimer  *time.Timer

	// Input-specific state

	completionEntries []string
	recentRecipients  []string
	rrIdx             int
	lastNickQuery     string

	// Follow means: when enabled, always keep the message list scrolled to newest.
	followEnabled bool

	pendingReply *pendingReply

	// Per chat view: stable pubkey→color for the session (switching away and back keeps colors).
	participantColorByView map[string]map[string]string
	participantHueByView   map[string]map[string]float64
	chatColorMu            sync.RWMutex

	pullingStatus      string // e.g. "#moscow"; shown in Info until messages arrive
	pullingStatusTimer *time.Timer
}

type chatListItem struct {
	viewIndex int
}

type outputMessage struct {
	Replyable    bool
	Nick         string
	ShortPubKey  string
	Content      string
	RawDisplay   string // full styled line(s) source for re-wrap on resize
	MentionToMe  bool   // highlight full row(s) — @you in message
	IsOwnMessage bool   // multiline wrap: continue input color on following rows
	// CreatedAt + SortID define chronological order (matches engine flushOrdered).
	CreatedAt int64
	SortID    string
}

// pendingReply holds the message being replied to (input title + send formatting).
type pendingReply struct {
	Nick        string
	ShortPubKey string
	Content     string
}

// New creates and initializes the entire TUI application.
func NewConsole(actions chan<- session.InboundIntent, events <-chan session.SurfaceUpdate) *console {
	t := &console{
		app:                    tview.NewApplication(),
		actionsChan:            actions,
		logsMaximized:          false,
		outputMaximized:        false,
		views:                  []session.RoomSpec{},
		relays:                 []session.RelayEndpoint{},
		selectedForGroup:       make(map[string]bool),
		activeViewIndex:        0,
		chatUsers:              []session.Participant{},
		chatUsersByPubKey:      make(map[string]session.Participant),
		completionEntries:      []string{},
		recentRecipients:       []string{},
		rrIdx:                  -1,
		lastNickQuery:          "",
		theme:                  defaultTheme,
		userPruneStopCh:        make(chan struct{}),
		followEnabled:          true,
		participantColorByView: make(map[string]map[string]string),
		participantHueByView:   make(map[string]map[string]float64),
	}

	t.setupViews()
	t.setupHandlers()
	t.updateInputLabel()
	t.app.SetRoot(t.mainFlex, true).SetFocus(t.input)
	t.updateFocusBorders()
	t.updateHints()
	t.updateDetailsView()

	go t.listenForEvents(events)
	go t.userPruner()

	return t
}

// logWriter is a helper to redirect the standard logger to the logs TextView.
type logWriter struct {
	textViewWriter io.Writer
	getColor       func() tcell.Color
}

func (lw *logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	ts := time.Now().Format("15:04:05")
	return fmt.Fprintf(lw.textViewWriter, "\n[%s][%s] %s[-]", lw.getColor(), ts, msg)
}

// Widget titles (short = narrow layout).
const (
	titleLogs          = "Logs (Alt+L)"
	titleChats         = "Chats (Alt+C)"
	titleUsers         = "Users (Alt+U)"
	titleMessages      = "Messages (Alt+M)"
	titleInput         = "Input (Alt+I)"
	titleLogsShort     = "Logs"
	titleChatsShort    = "Chats"
	titleUsersShort    = "Users"
	titleMessagesShort = "Messages"
	titleInputShort    = "Input"
)

// setupViews creates and configures all the visual primitives of the TUI.
func (t *console) setupViews() {
	t.applyTheme()
	t.initViews()
	t.initLayout()
}

// applyTheme sets the global styles for the application based on the current theme.
func (t *console) applyTheme() {
	tview.Styles.PrimitiveBackgroundColor = t.theme.backgroundColor
	tview.Styles.PrimaryTextColor = t.theme.textColor
	tview.Styles.BorderColor = t.theme.borderColor
	tview.Styles.TitleColor = t.theme.titleColor
}

func (t *console) followTitleSuffix() string {
	if t.followEnabled {
		return " Follow: ON"
	}
	return " Follow: OFF"
}

func (t *console) refreshFollowTitle() {
	// FOLLOW status must always be shown on the Messages window.
	if t.narrowMode {
		t.output.SetTitle(titleMessagesShort + t.followTitleSuffix())
	} else {
		t.output.SetTitle(titleMessages + t.followTitleSuffix())
	}
}

// jumpToLastMessage moves selection to the newest message (used when FOLLOW turns ON).
func (t *console) jumpToLastMessage() {
	if t.output == nil {
		return
	}
	n := t.output.GetItemCount()
	if n > 0 {
		t.output.SetCurrentItem(n - 1)
	}
}

// initViews initializes all the individual widgets for the TUI.
func (t *console) initViews() {
	t.logs = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(false).
		SetChangedFunc(func() {
			if t.logsMaximized && t.logsMaxList != nil {
				nOld := t.logsMaxList.GetItemCount()
				cur := t.logsMaxList.GetCurrentItem()
				atEnd := nOld > 0 && cur >= nOld-1
				t.rebuildMaximizedLogsList(atEnd)
			}
			t.app.Draw()
		})
	t.logs.SetBorder(true).SetTitle(titleLogs).SetTitleAlign(tview.AlignLeft)

	t.logsMaxList = tview.NewList().
		ShowSecondaryText(false).
		SetSelectedBackgroundColor(t.theme.borderColor).
		SetSelectedTextColor(t.theme.listSelectedFg).
		SetHighlightFullLine(true)
	t.logsMaxList.SetBorder(true)
	t.logsMaxList.SetTitle("Logs")
	t.logsMaxList.SetTitleAlign(tview.AlignLeft)
	t.logsMaxList.SetChangedFunc(func(int, string, string, rune) {
		t.refreshLogsFullscreenTitle()
	})
	customWriter := &logWriter{
		textViewWriter: tview.ANSIWriter(t.logs),
		getColor:       func() tcell.Color { return t.theme.logInfoColor },
	}
	log.SetOutput(customWriter)
	log.SetFlags(0)

	t.chatList = tview.NewList().
		ShowSecondaryText(false).
		SetSelectedBackgroundColor(t.theme.borderColor).
		SetSelectedTextColor(t.theme.listSelectedFg)
	t.chatList.SetBorder(true).SetTitleAlign(tview.AlignLeft)
	t.refreshChatListTitle()

	t.userList = tview.NewList().
		ShowSecondaryText(false).
		SetSelectedBackgroundColor(t.theme.borderColor).
		SetSelectedTextColor(t.theme.listSelectedFg)
	t.userList.SetBorder(true).SetTitleAlign(tview.AlignLeft)
	t.refreshUserListTitle()

	t.detailsView = tview.NewList().
		ShowSecondaryText(false).
		SetSelectedBackgroundColor(t.theme.borderColor).
		SetSelectedTextColor(t.theme.listSelectedFg)
	t.relaysFooter = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter).
		SetWrap(false)
	t.relaysPanel = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(t.detailsView, 0, 1, true).
		AddItem(t.relaysFooter, 2, 0, false)
	t.relaysPanel.SetBorder(true).SetTitleAlign(tview.AlignLeft)
	t.refreshRelaysPanelChrome()

	t.output = tview.NewList().
		ShowSecondaryText(false).
		// Hide selection highlight when messages are not focused.
		SetSelectedBackgroundColor(t.theme.backgroundColor).
		SetSelectedTextColor(t.theme.textColor).
		SetMainTextColor(t.theme.textColor).
		SetHighlightFullLine(false)
	t.output.SetBorder(true).SetTitle(titleMessages + t.followTitleSuffix()).SetTitleAlign(tview.AlignLeft)

	t.input = tview.NewInputField().
		SetLabelStyle(tcell.StyleDefault.Foreground(t.theme.titleColor)).
		SetFieldBackgroundColor(t.theme.inputBgColor).
		SetFieldTextColor(t.theme.inputTextColor)
	t.input.SetBorder(true).SetTitle(titleInput).SetTitleAlign(tview.AlignLeft)
	t.input.SetAutocompleteFunc(t.handleAutocomplete)
	t.input.SetAcceptanceFunc(func(textToCheck string, lastChar rune) bool {
		return graphemeLen(textToCheck) <= session.MaxMsgLen
	})
	t.input.SetChangedFunc(func(text string) {
		nick, complete := extractNickPrefix(text)
		if complete {
			t.lastNickQuery = ""
			return
		}
		if !complete && strings.Contains(text, "#") && t.lastNickQuery == "" {
			return
		}
		if nick != "" && nick != t.lastNickQuery {
			t.lastNickQuery = nick
			t.actionsChan <- session.InboundIntent{
				Type:    "REQUEST_NICK_COMPLETION",
				Payload: nick,
			}
		}
	})

	t.hints = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
}

// initLayout composes the widgets into the final layout and sets up responsiveness.
func (t *console) initLayout() {
	sidebarFlex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.chatList, 0, 1, true).
		AddItem(t.userList, 0, 1, false).
		AddItem(t.relaysPanel, 0, 2, false)

	t.contentGrid = tview.NewGrid().SetBorders(false)
	contentGrid := t.contentGrid

	t.wideComposerFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.input, 0, 1, true)
	t.wideBottomFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.wideComposerFlex, 0, 1, true).
		AddItem(t.hints, 1, 0, false)

	t.narrowComposerFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.input, 0, 1, true)
	t.narrowFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.output, 0, 1, false).
		AddItem(t.narrowComposerFlex, 1, 0, false)

	const narrowWidth = 100
	t.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		w, _ := screen.Size()
		contentGrid.Clear()

		desiredNarrow := w < narrowWidth

		// Debounce root switching so it doesn't happen repeatedly during a resize drag.
		t.resizeMu.Lock()
		t.desiredNarrowMode = desiredNarrow
		if desiredNarrow != t.narrowMode {
			if t.resizeApplyTimer != nil {
				t.resizeApplyTimer.Stop()
			}
			t.resizeApplyTimer = time.AfterFunc(150*time.Millisecond, func() {
				t.resizeMu.Lock()
				narrow := t.desiredNarrowMode
				t.resizeMu.Unlock()

				// Apply mode switch on UI goroutine.
				t.app.QueueUpdateDraw(func() {
					if narrow == t.narrowMode {
						return
					}
					if narrow {
						t.narrowMode = true
						t.logs.SetTitle(titleLogsShort).SetTitleAlign(tview.AlignLeft)
						t.output.SetTitle(titleMessagesShort + t.followTitleSuffix())
						t.refreshChatListTitle()
						t.refreshUserListTitle()
						t.updateDetailsView()
						t.updateInputLabel()
						t.restoreNarrowComposerNormal()
						t.app.SetRoot(t.narrowFlex, true).SetFocus(t.input)
					} else {
						t.narrowMode = false
						t.logs.SetTitle(titleLogs).SetTitleAlign(tview.AlignLeft)
						t.output.SetTitle(titleMessages + t.followTitleSuffix())
						t.refreshChatListTitle()
						t.refreshUserListTitle()
						t.updateDetailsView()
						t.input.SetTitle(titleInput)
						t.updateInputLabel()
						t.restoreWideComposerNormal()
						t.app.SetRoot(t.mainFlex, true)
					}
				})
			})
		}
		t.resizeMu.Unlock()

		nw := t.messagesWrapColumns(w)
		if nw != t.messagesCachedWrapW {
			t.messagesCachedWrapW = nw
			if len(t.outputMessages) > 0 {
				t.rebuildMessagesOutputPreservingSelection()
			}
		}

		// Configure grid only for wide mode; in narrow mode it's hidden by a different root.
		if !desiredNarrow {
			contentGrid.SetRows(0)
			// Fixed width of the right sidebar: chat list + users + details.
			// Keep it small enough so "Nick#hash" stays on one line, while
			// giving more horizontal room to the Messages area.
			contentGrid.SetColumns(0, 30)
			contentGrid.AddItem(t.output, 0, 0, 1, 1, 0, 0, false)
			contentGrid.AddItem(sidebarFlex, 0, 1, 1, 1, 0, 0, false)
		}
		return false
	})

	t.maximizedLogsFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.logsMaxList, 0, 1, true).
		AddItem(t.hints, 1, 0, false)

	t.maximizedOutputFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.output, 0, 1, true).
		AddItem(t.hints, 1, 0, false)

	t.mainFlex = tview.NewFlex().SetDirection(tview.FlexRow)
	t.rebuildMainFlexBottom(wideBottomRowsNormal)
}

const wideBottomRowsNormal = 4

func (t *console) formatContentWithNickMentions(content string) string {
	lookup := make(map[string]string, len(t.chatUsersByPubKey)*2+1)
	for pk, u := range t.chatUsersByPubKey {
		if u.Nick == "" || u.ShortPubKey == "" {
			continue
		}
		sh := strings.ToLower(u.ShortPubKey)
		if len(sh) > 4 {
			sh = sh[len(sh)-4:]
		}
		lookup[strings.ToLower(u.Nick)+"#"+sh] = pk
	}
	idxs := nickHashMentionIndices(content)
	if len(idxs) == 0 {
		return content
	}
	var b strings.Builder
	prev := 0
	for _, loc := range idxs {
		fullStart, fullEnd := loc[0], loc[1]
		nick := strings.ToLower(content[loc[2]:loc[3]])
		hsh := strings.ToLower(content[loc[4]:loc[5]])
		key := nick + "#" + hsh
		b.WriteString(content[prev:fullStart])
		if pk, ok := lookup[key]; ok {
			b.WriteString(t.colorTagForPubkey(pk))
			b.WriteString(content[fullStart:fullEnd])
			b.WriteString("[-]")
		} else {
			b.WriteString(t.colorTagForMentionNickHash(content[loc[2]:loc[3]], content[loc[4]:loc[5]]))
			b.WriteString(content[fullStart:fullEnd])
			b.WriteString("[-]")
		}
		prev = fullEnd
	}
	b.WriteString(content[prev:])
	return b.String()
}

// rebuildMainFlexBottom sets how many terminal rows the wide-mode input strip uses.
func (t *console) rebuildMainFlexBottom(bottomFixed int) {
	if t.mainFlex == nil || t.contentGrid == nil {
		return
	}
	t.mainFlex.Clear()
	t.mainFlex.AddItem(t.logs, 3, 0, false).
		AddItem(t.contentGrid, 0, 1, false).
		AddItem(t.wideBottomFlex, bottomFixed, 0, true)
}

// restoreWideComposerNormal puts single-line input back in the wide bottom area.
func (t *console) restoreWideComposerNormal() {
	t.wideComposerFlex.Clear()
	t.wideComposerFlex.AddItem(t.input, 0, 1, true)
	t.wideBottomFlex.Clear()
	t.wideBottomFlex.AddItem(t.wideComposerFlex, 0, 1, true).AddItem(t.hints, 1, 0, false)
	t.rebuildMainFlexBottom(wideBottomRowsNormal)
}

// restoreNarrowComposerNormal: chat + single-line input (narrow).
func (t *console) restoreNarrowComposerNormal() {
	t.narrowFlex.Clear()
	t.narrowFlex.AddItem(t.output, 0, 1, false)
	t.narrowComposerFlex.Clear()
	t.narrowComposerFlex.AddItem(t.input, 0, 1, true)
	t.narrowFlex.AddItem(t.narrowComposerFlex, 1, 0, false)
}

// handleAutocomplete provides completion entries for the input field.
func (t *console) handleAutocomplete(currentText string) []string {
	trimmed := strings.TrimSpace(currentText)

	if strings.HasPrefix(trimmed, "/block ") ||
		strings.HasPrefix(trimmed, "/unblock ") ||
		strings.HasPrefix(trimmed, "/b ") ||
		strings.HasPrefix(trimmed, "/ub ") {
		parts := strings.SplitN(currentText, " ", 2)
		if len(parts) < 2 {
			return nil
		}
		cmd := parts[0] + " "

		if len(t.completionEntries) == 0 {
			return nil
		}
		out := make([]string, 0, len(t.completionEntries))
		for _, e := range t.completionEntries {
			out = append(out, cmd+e)
		}
		return out
	}

	nick, complete := extractNickPrefix(currentText)
	if complete {
		t.completionEntries = nil
		return nil
	}
	if nick == "" {
		return nil
	}

	if len(t.completionEntries) == 0 {
		return nil
	}

	return append([]string(nil), t.completionEntries...)
}

// listenForEvents is the main event loop that processes events from the client.
func (t *console) listenForEvents(events <-chan session.SurfaceUpdate) {
	for event := range events {
		if event.Type == "SHUTDOWN" {
			// Stop background goroutines.
			select {
			case <-t.userPruneStopCh:
				// already closed
			default:
				close(t.userPruneStopCh)
			}
			break
		}

		t.app.QueueUpdateDraw(func() {
			switch event.Type {
			case "NEW_MESSAGE":
				t.handleNewMessage(event)
			case "INFO":
				t.handleInfoMessage(event)
			case "STATUS", "ERROR":
				t.handleLogMessage(event)
			case "STATE_UPDATE":
				t.handleStateUpdate(event)
			case "RELAYS_UPDATE":
				t.handleRelaysUpdate(event)
			case "NICK_COMPLETION_RESULT":
				t.handleNickCompletion(event)
			case "CHAT_USERS_UPDATE":
				t.handleChatUsersUpdate(event)
			case "CHAT_USER_DISCOVERED":
				t.handleChatUserDiscovered(event)
			}
		})
	}
	t.app.Stop()
}

func (t *console) userPruner() {
	// Periodically drop users whose last message is older than 3 minutes.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			t.app.QueueUpdateDraw(func() {
				t.pruneStaleChatUsers(now)
			})
		case <-t.userPruneStopCh:
			return
		}
	}
}

func (t *console) pruneStaleChatUsers(now time.Time) {
	if t.chatUsersByPubKey == nil || len(t.chatUsers) == 0 {
		return
	}

	cutoff := now.Add(-3 * time.Minute)
	newUsers := make([]session.Participant, 0, len(t.chatUsers))
	newByPubKey := make(map[string]session.Participant, len(t.chatUsersByPubKey))

	for _, u := range t.chatUsers {
		if u.PubKey == "" {
			continue
		}
		if u.LastMsgAt <= 0 {
			continue
		}
		last := time.Unix(u.LastMsgAt, 0)
		if last.Before(cutoff) {
			continue
		}
		newUsers = append(newUsers, u)
		newByPubKey[u.PubKey] = u
	}

	// Avoid churn: if nothing changed, don't redraw.
	if len(newUsers) == len(t.chatUsers) {
		t.chatUsersByPubKey = newByPubKey
		return
	}

	t.chatUsers = newUsers
	t.chatUsersByPubKey = newByPubKey
	t.updateUserList()
}

func findOutputMessageInsertIndex(msgs []outputMessage, createdAt int64, sortID string) int {
	for i := range msgs {
		m := &msgs[i]
		if createdAt < m.CreatedAt || (createdAt == m.CreatedAt && sortID < m.SortID) {
			return i
		}
	}
	return len(msgs)
}

// handleNewMessage processes and displays a new chat message.
func (t *console) handleNewMessage(event session.SurfaceUpdate) {
	if len(t.views) == 0 || t.activeViewIndex < 0 || t.activeViewIndex >= len(t.views) {
		return
	}

	activeView := t.views[t.activeViewIndex]
	showMessage := false
	if activeView.IsGroup {
		if slices.Contains(activeView.Children, event.Chat) {
			showMessage = true
		}
	} else {
		if event.Chat == activeView.Name {
			showMessage = true
		}
	}
	if !showMessage {
		return
	}

	clearedPulling := false
	if t.pullingStatus != "" {
		t.clearPullingMessagesStatus()
		clearedPulling = true
	}

	t.maybeUpsertChatUserFromEvent(event)

	selMsgIdx := -1
	if cur := t.output.GetCurrentItem(); cur >= 0 && cur < len(t.outputRowToMsg) {
		selMsgIdx = t.outputRowToMsg[cur]
	}

	// Smart follow: only select new message when user was already at bottom.
	itemCountBefore := t.output.GetItemCount()
	currentSelBefore := t.output.GetCurrentItem()
	shouldFollow := itemCountBefore == 0 || currentSelBefore == itemCountBefore-1
	if t.followEnabled {
		shouldFollow = true
	}

	nickColorTag := t.colorTagForPubkey(event.FullPubKey)
	ownColorTag := fmt.Sprintf("[%s]", t.theme.inputTextColor)
	ownNickTag := fmt.Sprintf("[%s::b]", t.theme.inputTextColor)

	label := ""
	if activeView.IsGroup {
		label = fmt.Sprintf("[%s]%s[-] ", t.theme.titleColor, event.Chat)
	}

	rawLine := strings.ReplaceAll(event.Content, "\n", " ")
	mentionMe := !event.IsOwnMessage && isSelfMentionedInContent(rawLine, t.nick, t.selfShortPubKey)

	content := rawLine
	if t.nick != "" && !mentionMe {
		content = highlightPlainMentionOfMe(content, t.nick, t.theme.inputTextColor)
	}
	content = t.formatContentWithNickMentions(content)

	mentionBody := rawLine
	if t.nick != "" {
		mentionBody = highlightPlainMentionOfMe(mentionBody, t.nick, t.theme.inputTextColor)
	}
	mentionBody = t.formatContentWithNickMentions(mentionBody)

	metaTag := colorHexTag(t.theme.logInfoColor)
	metaBracketed := fmt.Sprintf("[%s %s]", event.ID, event.Timestamp)
	var display string
	if event.IsOwnMessage {
		display = fmt.Sprintf(
			"%s%s%s#%s[-]> %s%s[-] %s%s[-]",
			label,
			ownNickTag, event.Nick,
			event.ShortPubKey,
			ownColorTag, content,
			metaTag, metaBracketed,
		)
	} else if mentionMe {
		display = fmt.Sprintf(
			"%s%s%s#%s[-]> %s %s%s[-]",
			label,
			nickColorTag, event.Nick,
			event.ShortPubKey,
			mentionBody,
			metaTag, metaBracketed,
		)
	} else {
		display = fmt.Sprintf(
			"%s%s%s#%s[-]> %s %s%s[-]",
			label,
			nickColorTag, event.Nick,
			event.ShortPubKey,
			content,
			metaTag, metaBracketed,
		)
	}

	// Nostr created_at is Unix seconds; normalize to nanoseconds so INFO lines (UnixNano) compare correctly.
	createdAtNs := event.CreatedAt * 1_000_000_000
	insertAt := findOutputMessageInsertIndex(t.outputMessages, createdAtNs, event.ID)
	if selMsgIdx >= 0 && insertAt <= selMsgIdx {
		selMsgIdx++
	}

	om := outputMessage{
		Replyable:    true,
		Nick:         event.Nick,
		ShortPubKey:  event.ShortPubKey,
		Content:      event.Content,
		RawDisplay:   display,
		MentionToMe:  mentionMe,
		IsOwnMessage: event.IsOwnMessage,
		CreatedAt:    createdAtNs,
		SortID:       event.ID,
	}
	t.outputMessages = slices.Insert(t.outputMessages, insertAt, om)

	preserveIdx := selMsgIdx
	if shouldFollow {
		preserveIdx = -1
	}
	t.rebuildMessagesOutput(preserveIdx)
	if clearedPulling {
		t.updateDetailsView()
	}
}

func (t *console) startPullingMessagesStatus() {
	if t.pullingStatusTimer != nil {
		t.pullingStatusTimer.Stop()
		t.pullingStatusTimer = nil
	}
	t.pullingStatus = ""
	if t.activeViewIndex < 0 || t.activeViewIndex >= len(t.views) {
		return
	}
	v := t.views[t.activeViewIndex]
	t.pullingStatus = "#" + strings.TrimPrefix(v.Name, "#")
	t.pullingStatusTimer = time.AfterFunc(12*time.Second, func() {
		t.app.QueueUpdateDraw(func() {
			t.clearPullingMessagesStatus()
			t.updateDetailsView()
		})
	})
}

func (t *console) clearPullingMessagesStatus() {
	t.pullingStatus = ""
	if t.pullingStatusTimer != nil {
		t.pullingStatusTimer.Stop()
		t.pullingStatusTimer = nil
	}
}

// clearMessagesWindow clears the visible Messages list (local UI only).
func (t *console) clearMessagesWindow() {
	if t.output == nil {
		return
	}
	t.output.Clear()
	t.outputMessages = nil
	t.outputRowToMsg = nil
	if t.output.GetItemCount() > 0 {
		t.output.SetCurrentItem(0)
	}
}

// handleInfoMessage displays a generic informational message in the output view.
func (t *console) handleInfoMessage(event session.SurfaceUpdate) {
	if t.output == nil {
		return
	}
	content := strings.TrimSpace(event.Content)
	disp := fmt.Sprintf("-- %s", content)
	now := time.Now().UnixNano()
	insertAt := findOutputMessageInsertIndex(t.outputMessages, now, "")
	t.outputMessages = slices.Insert(t.outputMessages, insertAt, outputMessage{
		Replyable:  false,
		Content:    content,
		RawDisplay: disp,
		CreatedAt:  now,
		SortID:     "",
	})
	t.rebuildMessagesOutput(-1)
}

// replyToSelectedMessage starts reply mode: title shows target; send prepends quote block.
func (t *console) replyToSelectedMessage() {
	if t.output == nil || t.outputMessages == nil {
		return
	}
	idx := t.output.GetCurrentItem()
	if idx < 0 || idx >= len(t.outputRowToMsg) {
		return
	}
	mid := t.outputRowToMsg[idx]
	if mid < 0 || mid >= len(t.outputMessages) {
		return
	}
	m := t.outputMessages[mid]
	if !m.Replyable {
		return
	}

	go func() {
		t.app.QueueUpdateDraw(func() {
			t.pendingReply = &pendingReply{
				Nick:        m.Nick,
				ShortPubKey: m.ShortPubKey,
				Content:     m.Content,
			}
			t.input.SetText("")
			t.updateInputLabel()
			t.app.SetFocus(t.input)
			t.updateHints()
			t.updateFocusBorders()
		})
	}()
}

func (t *console) clearPendingReply() {
	t.pendingReply = nil
	t.updateInputLabel()
}

// handleLogMessage displays a status or error message in the logs view.
func (t *console) handleLogMessage(event session.SurfaceUpdate) {
	color := t.theme.logWarnColor
	if event.Type == "ERROR" {
		color = t.theme.logErrorColor
	}
	fmt.Fprintf(t.logs, "\n[%s][%s] %s: %s[-]", color, time.Now().Format("15:04:05"), event.Type, event.Content)
	if !t.logsMaximized {
		t.logs.ScrollToEnd()
	}
}

// handleStateUpdate updates the TUI's state based on data from the client.
func (t *console) handleStateUpdate(event session.SurfaceUpdate) {
	state, ok := event.Payload.(session.LayoutSnapshot)
	if !ok {
		fmt.Fprintf(t.logs, "\n[%s]ERROR: Invalid STATE_UPDATE payload[-]", t.theme.logErrorColor)
		return
	}
	prevActiveIndex := t.activeViewIndex

	t.views = state.Views
	t.activeViewIndex = state.ActiveViewIndex
	t.nick = state.Nick
	t.selfShortPubKey = state.ShortPubKey

	// Clear visible message history when switching chats or reloading same chat (new identity).
	// Old messages will be re-rendered from the relay backlog (lookback+limit).
	if prevActiveIndex != t.activeViewIndex || state.ClearMessagePane {
		t.pendingReply = nil
		t.output.Clear()
		t.outputMessages = nil
		t.outputRowToMsg = nil
		t.output.SetCurrentItem(0)
		t.startPullingMessagesStatus()
	}

	t.updateChatList()
	t.updateDetailsView()
	t.updateInputLabel()

	// Reset and request users for the newly active view (per-view colors stay in participantColorByView).
	t.chatUsers = nil
	t.chatUsersByPubKey = make(map[string]session.Participant)
	t.updateUserList()
	t.requestChatUsersForActiveView()
}

// handleRelaysUpdate refreshes the list of relays.
func (t *console) handleRelaysUpdate(event session.SurfaceUpdate) {
	switch p := event.Payload.(type) {
	case session.RelayPanelSnapshot:
		t.relays = p.Relays
		t.relaysUpCount = p.UpCount
		t.relaysDownCount = p.DownCount
	case []session.RelayEndpoint:
		t.relays = p
		t.relaysUpCount = 0
		t.relaysDownCount = 0
		for _, r := range p {
			if r.Connected {
				t.relaysUpCount++
			} else {
				t.relaysDownCount++
			}
		}
	default:
		fmt.Fprintf(t.logs, "\n[%s]ERROR: Invalid RELAYS_UPDATE payload[-]", t.theme.logErrorColor)
		return
	}
	t.updateDetailsView()
}

func (t *console) handleChatUsersUpdate(event session.SurfaceUpdate) {
	users, ok := event.Payload.([]session.Participant)
	if !ok {
		return
	}

	t.chatUsers = users
	t.chatUsersByPubKey = make(map[string]session.Participant, len(users))
	for _, u := range users {
		t.chatUsersByPubKey[u.PubKey] = u
	}
	// Immediately prune based on last message timestamps.
	t.pruneStaleChatUsers(time.Now())
	t.ensureParticipantColorsFromUsers()
	t.updateUserList()
}

func (t *console) handleChatUserDiscovered(event session.SurfaceUpdate) {
	u, ok := event.Payload.(session.Participant)
	if !ok {
		return
	}

	// Only show users for the currently active chat scope.
	if len(t.views) == 0 || t.activeViewIndex < 0 || t.activeViewIndex >= len(t.views) {
		return
	}
	activeView := t.views[t.activeViewIndex]
	if activeView.IsGroup {
		if !slices.Contains(activeView.Children, u.Chat) {
			return
		}
	} else {
		if u.Chat != activeView.Name {
			return
		}
	}

	if t.chatUsersByPubKey == nil {
		t.chatUsersByPubKey = make(map[string]session.Participant)
	}

	if existing, exists := t.chatUsersByPubKey[u.PubKey]; exists {
		// Update any new nick/hash information.
		existing.Nick = u.Nick
		existing.ShortPubKey = u.ShortPubKey
		existing.Chat = u.Chat
		existing.LastMsgAt = u.LastMsgAt
		t.chatUsersByPubKey[u.PubKey] = existing
	} else {
		t.chatUsers = append(t.chatUsers, u)
		t.chatUsersByPubKey[u.PubKey] = u
		t.assignNewParticipantColor(u.PubKey)
	}

	t.updateUserList()
}

func (t *console) requestChatUsersForActiveView() {
	if len(t.views) == 0 || t.activeViewIndex < 0 || t.activeViewIndex >= len(t.views) {
		return
	}
	activeView := t.views[t.activeViewIndex]
	if activeView.Name == "" {
		return
	}

	t.actionsChan <- session.InboundIntent{
		Type:    "REQUEST_CHAT_USERS",
		Payload: activeView.Name,
	}
}

func (t *console) maybeUpsertChatUserFromEvent(event session.SurfaceUpdate) {
	if event.FullPubKey == "" {
		return
	}
	if len(t.views) == 0 || t.activeViewIndex < 0 || t.activeViewIndex >= len(t.views) {
		return
	}

	activeView := t.views[t.activeViewIndex]
	isRelevant := false
	if activeView.IsGroup {
		isRelevant = slices.Contains(activeView.Children, event.Chat)
	} else {
		isRelevant = event.Chat == activeView.Name
	}
	if !isRelevant {
		return
	}

	u := session.Participant{
		PubKey:      event.FullPubKey,
		Nick:        event.Nick,
		ShortPubKey: event.ShortPubKey,
		LastMsgAt:   event.CreatedAt,
	}

	if existing, ok := t.chatUsersByPubKey[event.FullPubKey]; ok {
		// Update timestamp on every message so pruning is correct.
		existing.Nick = u.Nick
		existing.ShortPubKey = u.ShortPubKey
		existing.LastMsgAt = u.LastMsgAt
		t.chatUsersByPubKey[event.FullPubKey] = existing

		for i := range t.chatUsers {
			if t.chatUsers[i].PubKey == event.FullPubKey {
				t.chatUsers[i] = existing
				break
			}
		}
		return
	}

	t.chatUsers = append(t.chatUsers, u)
	t.chatUsersByPubKey[event.FullPubKey] = u
	t.assignNewParticipantColor(event.FullPubKey)
	t.updateUserList()
}

// inputShouldReceiveTabForAutocomplete is true when Tab should go to the input (pick completion)
// instead of cycling focus — global capture normally eats Tab before tview sees it.
func (t *console) inputShouldReceiveTabForAutocomplete() bool {
	if t.app.GetFocus() != t.input {
		return false
	}
	text := t.input.GetText()
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/block ") ||
		strings.HasPrefix(trimmed, "/unblock ") ||
		strings.HasPrefix(trimmed, "/b ") ||
		strings.HasPrefix(trimmed, "/ub ") {
		parts := strings.SplitN(text, " ", 2)
		return len(parts) >= 2 && len(t.completionEntries) > 0
	}
	nick, complete := extractNickPrefix(text)
	return !complete && nick != "" && len(t.completionEntries) > 0
}

// handleNickCompletion provides completion entries to the input field.
func (t *console) handleNickCompletion(event session.SurfaceUpdate) {
	entries, ok := event.Payload.([]string)
	if !ok {
		return
	}
	t.completionEntries = entries
	if len(entries) == 1 {
		text := t.input.GetText()
		nick, complete := extractNickPrefix(text)
		if !complete && nick != "" {
			lastAt := strings.LastIndex(text, "@")
			if lastAt >= 0 {
				ent := strings.TrimSpace(entries[0])
				ent = strings.TrimPrefix(ent, "@")
				cname, _, _ := strings.Cut(ent, "#")
				if strings.HasPrefix(strings.ToLower(cname), strings.ToLower(nick)) {
					t.input.SetText(text[:lastAt] + entries[0])
					t.completionEntries = nil
					return
				}
			}
		}
	}
	t.input.Autocomplete()
}

// applyOwnMessageMultilineGreen restyles wrapped continuation rows: each List row is
// drawn separately, so text after a line break must re-open the input (green) color.
func (t *console) applyOwnMessageMultilineGreen(lines []string) {
	if len(lines) <= 1 {
		return
	}
	tag := fmt.Sprintf("[%s]", t.theme.inputTextColor)
	for i := 1; i < len(lines); i++ {
		lines[i] = tag + lines[i]
		if i < len(lines)-1 {
			lines[i] += "[-]"
		}
	}
}

func (t *console) logsLineSlice() []string {
	raw := strings.TrimRight(t.logs.GetText(true), "\n")
	if raw == "" {
		return []string{""}
	}
	return strings.Split(raw, "\n")
}

func (t *console) rebuildMaximizedLogsList(scrollToEnd bool) {
	if t.logsMaxList == nil {
		return
	}
	nOld := t.logsMaxList.GetItemCount()
	cur := t.logsMaxList.GetCurrentItem()
	atEnd := scrollToEnd || (nOld > 0 && cur >= nOld-1)

	lines := t.logsLineSlice()
	t.logsMaxList.Clear()
	for _, ln := range lines {
		s := ln
		if s == "" {
			s = " "
		}
		t.logsMaxList.AddItem(s, "", 0, nil)
	}
	n := t.logsMaxList.GetItemCount()
	if n == 0 {
		t.logsMaxList.AddItem("(empty)", "", 0, nil)
		n = 1
	}
	if atEnd {
		t.logsMaxList.SetCurrentItem(n - 1)
	} else if cur >= 0 && cur < n {
		t.logsMaxList.SetCurrentItem(cur)
	} else {
		t.logsMaxList.SetCurrentItem(max(0, n-1))
	}
	t.refreshLogsFullscreenTitle()
}

func (t *console) refreshLogsFullscreenTitle() {
	if !t.logsMaximized || t.logsMaxList == nil {
		return
	}
	n := t.logsMaxList.GetItemCount()
	if n < 1 {
		n = 1
	}
	sel := t.logsMaxList.GetCurrentItem()
	if sel < 0 {
		sel = 0
	}
	t.logsMaxList.SetTitle(fmt.Sprintf("Logs %d/%d · c copy · ` exit", sel+1, n))
	t.logsMaxList.SetTitleAlign(tview.AlignLeft)
}

func (t *console) initLogsFullscreenSelection() {
	t.rebuildMaximizedLogsList(true)
	t.app.SetFocus(t.logsMaxList)
	t.updateFocusBorders()
	t.refreshLogsFullscreenTitle()
}

func (t *console) refreshLogsTitleForLayout() {
	if t.narrowMode {
		t.logs.SetTitle(titleLogsShort)
	} else {
		t.logs.SetTitle(titleLogs)
	}
	t.logs.SetTitleAlign(tview.AlignLeft)
}

// Run starts the TUI application.
func (t *console) Run() error {
	return t.app.Run()
}

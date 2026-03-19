package session

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// User Blocking

func (e *engine) blockUser(payload string) {
	var pkToBlock, nickToBlock string

	for _, pk := range e.identityCache.Keys() {
		if ctx, ok := e.identityCache.Get(pk); ok {
			userIdentifier := fmt.Sprintf("@%s#%s", ctx.nick, ctx.shortPubKey)
			if strings.HasPrefix(userIdentifier, payload) {
				pkToBlock = pk
				nickToBlock = fmt.Sprintf("%s#%s", ctx.nick, ctx.shortPubKey)
				break
			}
		}
	}

	if pkToBlock == "" {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Could not find user matching '%s' to block.", payload)}
		return
	}

	for _, blockedPeer := range e.prefs.BlockedUsers {
		if blockedPeer.PubKey == pkToBlock {
			e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("User %s is already blocked.", nickToBlock)}
			return
		}
	}

	e.prefs.BlockedUsers = append(e.prefs.BlockedUsers, blockedPeer{PubKey: pkToBlock, Nick: nickToBlock})
	e.persistPrefs()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Blocked user %s. Their messages will now be hidden.", nickToBlock)}
}

func (e *engine) unblockUser(payload string) {
	idxToRemove := -1

	if num, err := strconv.Atoi(payload); err == nil {
		if num > 0 && num <= len(e.prefs.BlockedUsers) {
			idxToRemove = num - 1
		} else {
			e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Invalid number: %d. Use '/block' to see the list.", num)}
			return
		}
	} else {
		cleanPayload := strings.TrimPrefix(payload, "@")
		for i, blockedPeer := range e.prefs.BlockedUsers {
			if strings.HasPrefix(blockedPeer.Nick, cleanPayload) || strings.HasPrefix(blockedPeer.PubKey, payload) {
				idxToRemove = i
				break
			}
		}
	}

	if idxToRemove == -1 {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Could not find a blocked user matching '%s'.", payload)}
		return
	}

	unblockedNick := e.prefs.BlockedUsers[idxToRemove].Nick
	if unblockedNick == "" {
		unblockedNick = e.prefs.BlockedUsers[idxToRemove].PubKey[:8] + "..."
	}

	e.prefs.BlockedUsers = append(e.prefs.BlockedUsers[:idxToRemove], e.prefs.BlockedUsers[idxToRemove+1:]...)
	e.persistPrefs()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Unblocked user %s.", unblockedNick)}
}

func (e *engine) listBlockedUsers() {
	if len(e.prefs.BlockedUsers) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: "Your block list is empty. Use /block <@nick> to block someone."}
		return
	}

	var builder strings.Builder
	builder.WriteString("Blocked Users:\n")
	for i, user := range e.prefs.BlockedUsers {
		nick := user.Nick
		if nick == "" {
			nick = "(no nick saved)"
		}
		builder.WriteString(fmt.Sprintf("[%d] - %s (%s...)\n", i+1, nick, user.PubKey[:8]))
	}
	e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: builder.String()}
}

// Filter Management

func (e *engine) handleFilter(payload string) {
	if payload == "" {
		e.listFilters()
		return
	}

	if idx, err := strconv.Atoi(payload); err == nil {
		e.toggleFilter(idx)
		return
	}

	e.addFilter(payload)
}

func (e *engine) addFilter(p string) {
	newFilter := textRule{Pattern: p, Enabled: true}
	e.prefs.Filters = append(e.prefs.Filters, newFilter)
	e.persistPrefs()
	e.rebuildRegexCaches()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Added and enabled filter: " + p}
}

func (e *engine) toggleFilter(idx int) {
	if idx < 1 || idx > len(e.prefs.Filters) {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Invalid filter number: %d. Use '/filter' to see the list.", idx)}
		return
	}
	filterIndex := idx - 1

	e.prefs.Filters[filterIndex].Enabled = !e.prefs.Filters[filterIndex].Enabled

	e.persistPrefs()
	e.rebuildRegexCaches()

	status := "disabled"
	if e.prefs.Filters[filterIndex].Enabled {
		status = "enabled"
	}
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Filter %d (%s) is now %s.", idx, e.prefs.Filters[filterIndex].Pattern, status)}
}

func (e *engine) listFilters() {
	if len(e.prefs.Filters) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: "No filters set."}
		return
	}
	var b strings.Builder
	b.WriteString("\nFilters:")
	for i, f := range e.prefs.Filters {
		var statusSymbol string
		if f.Enabled {
			statusSymbol = "+"
		} else {
			statusSymbol = "-"
		}
		b.WriteString(fmt.Sprintf("\n[%d] %s %s", i+1, statusSymbol, f.Pattern))
	}
	e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: b.String()}
}

func (e *engine) removeFilter(p string) {
	if p == "" {
		e.clearFilters()
		return
	}
	idx, err := strconv.Atoi(p)
	if err != nil || idx < 1 || idx > len(e.prefs.Filters) {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Invalid filter number."}
		return
	}
	removed := e.prefs.Filters[idx-1].Pattern
	e.prefs.Filters = append(e.prefs.Filters[:idx-1], e.prefs.Filters[idx:]...)
	e.persistPrefs()
	e.rebuildRegexCaches()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Removed filter: " + removed}
}

func (e *engine) clearFilters() {
	e.prefs.Filters = []textRule{}
	e.persistPrefs()
	e.rebuildRegexCaches()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Cleared all filters."}
}

// Mute Management

func (e *engine) handleMute(payload string) {
	if payload == "" {
		e.listMutes()
		return
	}
	if idx, err := strconv.Atoi(payload); err == nil {
		e.toggleMute(idx)
		return
	}
	e.addMute(payload)
}

func (e *engine) addMute(p string) {
	newMute := textRule{Pattern: p, Enabled: true}
	e.prefs.Mutes = append(e.prefs.Mutes, newMute)
	e.persistPrefs()
	e.rebuildRegexCaches()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Muted and enabled: " + p}
}

func (e *engine) toggleMute(idx int) {
	if idx < 1 || idx > len(e.prefs.Mutes) {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: fmt.Sprintf("Invalid mute number: %d. Use '/mute' to see the list.", idx)}
		return
	}
	muteIndex := idx - 1

	e.prefs.Mutes[muteIndex].Enabled = !e.prefs.Mutes[muteIndex].Enabled
	e.persistPrefs()
	e.rebuildRegexCaches()

	status := "disabled"
	if e.prefs.Mutes[muteIndex].Enabled {
		status = "enabled"
	}
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: fmt.Sprintf("Mute %d (%s) is now %s.", idx, e.prefs.Mutes[muteIndex].Pattern, status)}
}

func (e *engine) listMutes() {
	if len(e.prefs.Mutes) == 0 {
		e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: "No mutes set."}
		return
	}
	var b strings.Builder
	b.WriteString("\nMutes:")
	for i, m := range e.prefs.Mutes {
		var statusSymbol string
		if m.Enabled {
			statusSymbol = "+"
		} else {
			statusSymbol = "-"
		}
		b.WriteString(fmt.Sprintf("\n[%d] %s %s", i+1, statusSymbol, m.Pattern))
	}
	e.eventsChan <- SurfaceUpdate{Type: "INFO", Content: b.String()}
}

func (e *engine) removeMute(p string) {
	if p == "" {
		e.clearMutes()
		return
	}
	idx, err := strconv.Atoi(p)
	if err != nil || idx < 1 || idx > len(e.prefs.Mutes) {
		e.eventsChan <- SurfaceUpdate{Type: "ERROR", Content: "Invalid mute number."}
		return
	}
	removed := e.prefs.Mutes[idx-1].Pattern
	e.prefs.Mutes = append(e.prefs.Mutes[:idx-1], e.prefs.Mutes[idx:]...)
	e.persistPrefs()
	e.rebuildRegexCaches()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Removed mute: " + removed}
}

func (e *engine) clearMutes() {
	e.prefs.Mutes = []textRule{}
	e.persistPrefs()
	e.rebuildRegexCaches()
	e.eventsChan <- SurfaceUpdate{Type: "STATUS", Content: "Cleared all mutes."}
}

// Helpers

func (e *engine) rebuildRegexCaches() {
	compileAll := func(src []textRule) []matchRule {
		out := make([]matchRule, 0, len(src))
		for _, item := range src {
			if item.Enabled {
				out = append(out, compilePattern(item.Pattern))
			}
		}
		return out
	}
	e.filtersCompiled = compileAll(e.prefs.Filters)
	e.mutesCompiled = compileAll(e.prefs.Mutes)
}

func compilePattern(p string) matchRule {
	p = strings.TrimSpace(p)
	if len(p) > 1 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/") {
		body := p[1 : len(p)-1]
		if re, err := regexp.Compile(body); err == nil {
			return matchRule{raw: p, regex: re}
		}
		return matchRule{raw: p, literal: body}
	}
	return matchRule{raw: p, literal: p}
}

func (e *engine) matchesAny(content string, patterns []matchRule) bool {
	for _, pat := range patterns {
		if pat.regex != nil {
			if pat.regex.MatchString(content) {
				return true
			}
		} else if pat.literal != "" {
			if strings.Contains(content, pat.literal) {
				return true
			}
		}
	}
	return false
}

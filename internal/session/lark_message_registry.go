package session

import "sync"

var defaultLarkMessageRegistry = &LarkMessageRegistry{messageToSession: make(map[string]string)}

type LarkMessageRegistry struct {
	mu               sync.RWMutex
	messageToSession map[string]string
	latestSessionID  string
	chatToSession    map[string]string
	botInputs        map[string]bool
}

func (r *LarkMessageRegistry) rememberInput(messageID, senderType string) {
	if messageID == "" || senderType == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.botInputs == nil {
		r.botInputs = make(map[string]bool)
	}
	r.botInputs[messageID] = senderType != "" && senderType != "user"
}

func (r *LarkMessageRegistry) isBotInput(messageID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.botInputs[messageID]
}

func (r *LarkMessageRegistry) remember(sessionID string, messageIDs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.messageToSession == nil {
		r.messageToSession = make(map[string]string)
	}
	for _, id := range messageIDs {
		if id != "" {
			r.messageToSession[id] = sessionID
		}
	}
	if sessionID != "" {
		r.latestSessionID = sessionID
	}
}

func (r *LarkMessageRegistry) rememberChat(chatID string, sessionID string) {
	if chatID == "" || sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.chatToSession == nil {
		r.chatToSession = make(map[string]string)
	}
	r.chatToSession[chatID] = sessionID
	r.latestSessionID = sessionID
}

func (r *LarkMessageRegistry) lookupChat(chatID string) (string, bool) {
	if chatID == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	sessionID, ok := r.chatToSession[chatID]
	return sessionID, ok
}

func (r *LarkMessageRegistry) forgetChat(chatID string, sessionID string) {
	if chatID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.chatToSession == nil {
		return
	}
	if current, ok := r.chatToSession[chatID]; ok && (sessionID == "" || current == sessionID) {
		delete(r.chatToSession, chatID)
	}
}

func (r *LarkMessageRegistry) lookup(messageIDs ...string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, id := range messageIDs {
		if id == "" {
			continue
		}
		if sess, ok := r.messageToSession[id]; ok {
			return sess, true
		}
	}
	return "", false
}

func (r *LarkMessageRegistry) rememberLatest(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latestSessionID = sessionID
}

func (r *LarkMessageRegistry) latestNotifiedSessionID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.latestSessionID
}

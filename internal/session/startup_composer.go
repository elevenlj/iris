package session

import (
	"context"
	"log"
	"time"
)

// Startup animations can emit output forever. Probe on a fixed cadence, not
// a silence debounce; only a fresh rendered composer can release queued input.
func (rt *RuntimeSession) scheduleStartupComposerProbeLocked() {
	if rt.manager == nil || rt.closed || !rt.session.Live || rt.startupNotifyMode != startupNotifyDiscard || rt.startupNotifyTimer != nil {
		return
	}
	kind := agentKindForCommand(rt.session.LastAgentStartCommand, rt.session.LastAgentKind)
	if !isCodexFamily(kind) && kind != "claude" && kind != "aiden" {
		return // Unknown custom CLIs have no reliable composer detector.
	}
	rt.startupNotifyTimer = time.AfterFunc(time.Second, rt.probeStartupComposer)
}

func (rt *RuntimeSession) probeStartupComposer() {
	rt.beginStartupNotification("")
	fresh := rt.RequestFreshSnapshot(defaultNotifySnapshotTimeout)
	rt.mu.Lock()
	rt.startupNotifyTimer = nil
	if rt.closed || !rt.session.Live || rt.startupNotifyMode != startupNotifyDiscard {
		rt.mu.Unlock()
		return
	}
	kind := agentKindForCommand(rt.session.LastAgentStartCommand, rt.session.LastAgentKind)
	if !fresh || rt.startupNotificationCreating || rt.agentRestartPending || rt.controlInputActive || !startupAgentComposerReady(rt.visibleSnapshot, rt.visibleSnapshotSource, kind) {
		rt.scheduleStartupComposerProbeLocked()
		rt.mu.Unlock()
		return
	}
	rt.stopNotifyTimerLocked()
	rt.stopNotifyStableTimerLocked()
	rt.startupNotifyMode = startupNotifyNormal
	rt.startupComposerIdle = true
	rt.terminalMenuActive = false
	rt.pendingTerminalInteraction = nil
	rt.session.Status = StatusWaiting
	rt.session.UpdatedAt = time.Now().UTC()
	rt.stateVersion++
	rt.notifyVersion++
	sess := rt.session
	rt.mu.Unlock()
	_ = rt.manager.persist(context.Background(), sess)
	rt.completeStartupNotification()
	log.Printf("startup composer ready session=%s agent=%s", sess.ID, kind)
	rt.manager.notificationSent(sess.ID)
}

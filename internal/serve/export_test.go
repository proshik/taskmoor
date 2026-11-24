package serve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SetManagedSettings points the managed Claude settings at files.
func SetManagedSettings(files ...string) func() {
	old := managedSettingsFiles
	managedSettingsFiles = func() []string { return files }
	return func() { managedSettingsFiles = old }
}

// SetACPTimeouts shortens the ACP setup deadlines for a test.
func SetACPTimeouts(d time.Duration) func() {
	i, s := acpInitTimeout, acpSetupTimeout
	acpInitTimeout, acpSetupTimeout = d, d
	return func() { acpInitTimeout, acpSetupTimeout = i, s }
}

// TelegramPass runs the retry and reminder pass now.
func (s *Service) TelegramPass() { s.sendOpenAsks(context.Background(), true) }

// TelegramSent says whether an ask's message is recorded as delivered
// and its send is over.
func (s *Service) TelegramSent(id string) bool {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	_, ok := s.tg.sent[id]
	_, busy := s.tg.sending[id]
	return ok && !busy
}

// TelegramFailed says whether an ask's send failed and is over.
func (s *Service) TelegramFailed(id string) bool {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	_, ok := s.tg.tried[id]
	_, busy := s.tg.sending[id]
	return ok && !busy
}

// AnswerIf answers an ask's first question with option n if still() holds
// as it is taken.
func (s *Service) AnswerIf(id string, n int, still func() bool) error {
	return s.answerIf(id, Answer{Choices: [][]string{{fmt.Sprint(n)}}}, "person", "", still)
}

// SetApproving writes an Approve's intent, as Approve does before main moves.
func (s *Service) SetApproving(id string, ap Approval) {
	_, _ = s.changeIdea(id, func(i *Idea) error { i.Approving = &ap; return nil })
}

// ClearCeremonies forgets the confirmations under way.
func (s *Service) ClearCeremonies() {
	s.rm.mu.Lock()
	s.rm.ceremonies = map[string]*ceremony{}
	s.rm.mu.Unlock()
}

// SetRemoteStreamTTL bounds remote event streams; the returned func puts it back.
func SetRemoteStreamTTL(d time.Duration) func() {
	old := remoteStreamTTL
	remoteStreamTTL = d
	return func() { remoteStreamTTL = old }
}

// SetPushTarget lets the tests' push service in (any host, its TLS); the
// returned func puts both back.
func SetPushTarget(rt http.RoundTripper) func() {
	oldOK, oldRT := pushHostOK, pushClient.Transport
	pushHostOK = func(e string) bool { return strings.HasPrefix(e, "https://127.0.0.1:") }
	pushClient.Transport = rt
	return func() { pushHostOK, pushClient.Transport = oldOK, oldRT }
}

// SetPushQueueMax bounds the push queue; the func puts it back.
func (s *Service) SetPushQueueMax(n int) func() {
	s.push.mu.Lock()
	old := s.push.limit
	s.push.limit = n
	s.push.mu.Unlock()
	return func() { s.push.mu.Lock(); s.push.limit = old; s.push.mu.Unlock() }
}

// PushKick wakes the push sender.
func (s *Service) PushKick() {
	select {
	case s.push.wake <- struct{}{}:
	default:
	}
}

// SetPushWriteFail makes push.json unwritable (nil: writable again).
func (s *Service) SetPushWriteFail(err error) {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if err == nil {
		s.push.writeFail = nil
		return
	}
	s.push.writeFail = func() error { return err }
}

// PushGC forgets revoked devices' subscriptions now.
func (s *Service) PushGC() { s.pushGC() }

// PushSubscriptions counts the subscriptions kept.
func (s *Service) PushSubscriptions() int {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	_ = s.pushLoad()
	return len(s.push.subs)
}

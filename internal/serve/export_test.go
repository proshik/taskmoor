package serve

import (
	"context"
	"fmt"
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

package serve

import "time"

// agentRunner is one running agent process, of any harness: Claude's
// stream-json runner or an ACP runner (Codex, OpenCode). The service
// talks to every harness through it; the harness's own events reach the
// service through the claudeHost callbacks.
type agentRunner interface {
	// send delivers an instruction to the running conversation.
	send(text string) error
	// allow and deny answer a pending permission or question request;
	// for a question, input carries the answers (answerInput).
	allow(requestID string, input map[string]any) error
	deny(requestID, message string) error
	// stop ends the turn and the process; kill ends it at once.
	stop(grace time.Duration)
	kill()
	// closeInput ends the conversation: the process exits after the turn.
	closeInput()
	inputClosed() bool
	// finished is closed after the host's last callback.
	finished() <-chan struct{}
}

func (r *claudeRunner) finished() <-chan struct{} { return r.done }

// answerChecker is a runner that can refuse an answer before the ask is
// taken (an ACP form's typed fields).
type answerChecker interface {
	check(requestID string, input map[string]any) error
}

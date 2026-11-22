package serve

import "time"

// SetACPTimeouts shortens the ACP setup deadlines for a test.
func SetACPTimeouts(d time.Duration) func() {
	i, s := acpInitTimeout, acpSetupTimeout
	acpInitTimeout, acpSetupTimeout = d, d
	return func() { acpInitTimeout, acpSetupTimeout = i, s }
}

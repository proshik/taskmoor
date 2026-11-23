package serve

import "time"

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

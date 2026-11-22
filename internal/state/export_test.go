package state

// SetBeforePublish sets the test hook run just before publishing.
func SetBeforePublish(f func()) (restore func()) {
	old := beforePublish
	beforePublish = f
	return func() { beforePublish = old }
}

// SetSyncDir replaces the directory fsync.
func SetSyncDir(f func(string) error) (restore func()) {
	old := syncDir
	syncDir = f
	return func() { syncDir = old }
}

package usercopy

import "fmt"

// SnapshotCacheReadFailed is the sentence for an ETF holdings cache folder
// that couldn't be read. The folder is inside the broker, not on the
// user's disk, so the step is a report.
func SnapshotCacheReadFailed(dir, cause string) string {
	return fmt.Sprintf("Couldn't read the saved ETF holdings in %s: %s. %s", dir, cause, ReportIt)
}

// SnapshotCacheWriteFailed is the sentence for downloaded ETF holdings that
// couldn't be saved to the cache folder.
func SnapshotCacheWriteFailed(dir, cause string) string {
	return fmt.Sprintf("Couldn't save the downloaded ETF holdings in %s: %s. %s", dir, cause, ReportIt)
}

package web

// Exports for *_test.go in package web_test.

// SetOpenInFileManagerForTest swaps the platform-specific folder-opener with a
// test stub. Returns the previously installed function so callers can restore.
func SetOpenInFileManagerForTest(fn func(string) error) func(string) error {
	prev := openInFileManager
	openInFileManager = fn
	return prev
}

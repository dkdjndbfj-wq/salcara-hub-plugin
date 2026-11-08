package standalone

import "os"

// Windows cannot replace an existing directory with MoveFileEx.
func publishRestoredDirectory(stage, target string) error { return os.Rename(stage, target) }

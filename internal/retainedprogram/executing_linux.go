package retainedprogram

import "os"

// The kernel reference cannot be redirected by replacing the executable pathname.
func executingFile() (*os.File, error) { return os.Open("/proc/self/exe") }

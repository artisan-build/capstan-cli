package work

import "os"

// Lock is one acquired non-blocking overlap lock.
type Lock struct {
	file *os.File
}

package boot

import "golang.org/x/sys/unix"

// atomicExchange swaps the entries at a and b in one step (renameat2
// RENAME_EXCHANGE): at no instant is either name missing.
func atomicExchange(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}

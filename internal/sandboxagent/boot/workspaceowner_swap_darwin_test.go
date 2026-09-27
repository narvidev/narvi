package boot

import "golang.org/x/sys/unix"

// atomicExchange swaps the entries at a and b in one step (renamex_np
// RENAME_SWAP): at no instant is either name missing.
func atomicExchange(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}

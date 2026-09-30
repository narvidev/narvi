package review

// InputRead is how the reads behind a review's context ended: the pull
// request itself (its head, its size, its labels) and its diff (the changed
// paths). Set by the one producer of PreFetchedContext
// (internal/app/reviewcontext.Fetch), or by a review lane that never
// called it, and carried as a value to the depth decision (§26.3): an
// empty diff alone cannot say whether the change is empty or the read
// failed, and the two must route differently.
//
// The zero value is not a member. A context whose producer never set it
// reads as not readable (Readable below), the same as a failed read, so a
// lane that forgets to say what happened routes deep rather than light.
type InputRead string

const (
	// InputReadComplete means the pull request and its whole diff were read.
	InputReadComplete InputRead = "complete"
	// InputReadEmpty means both were read, and the change is genuinely empty
	// (the diff is empty and the pull request reports no changed files).
	InputReadEmpty InputRead = "empty"
	// InputReadNotFetched means the lane made no read at all (no fetcher wired,
	// GitHub outbound off, or a repository name that is not owner/repo).
	InputReadNotFetched InputRead = "not_fetched"
	// InputReadPRUnreadable means the pull request could not be read, so its
	// size, head and labels are unknown and no diff was fetched.
	InputReadPRUnreadable InputRead = "pr_unreadable"
	// InputReadDiffUnreadable means the pull request was read but its diff was
	// not: the fetch failed, came back empty while the pull request reports
	// changed files, or named no file at all.
	InputReadDiffUnreadable InputRead = "diff_unreadable"
	// InputReadDiffTruncated means the diff was read but its file list is
	// incomplete: cut at the response-size cap, or naming fewer files than
	// the pull request reports.
	InputReadDiffTruncated InputRead = "diff_truncated"
)

// Readable reports whether the reads left the change's size and changed
// paths known: complete, or genuinely empty. Every other value, the zero
// value included, is an unreadable input.
func (r InputRead) Readable() bool {
	return r == InputReadComplete || r == InputReadEmpty
}

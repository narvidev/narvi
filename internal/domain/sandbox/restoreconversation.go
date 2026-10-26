package sandbox

// A restore brings back the snapshot's filesystem, the agent runtime's own
// session storage among it (technical plan §3.3, §35.5). Nothing checks
// that the snapshot holds the conversation the session recorded last: the
// runtime adapter echoes the id it is sent. ClassifyRestoreConversation
// says how the two compare when the restore is claimed, and its caller
// counts it; when the snapshot's own conversation was recorded at its
// mint, the caller then makes the restored gen's prompts name that one,
// the conversation the restored runtime holds. A restore whose snapshot's
// conversation was not recorded keeps naming the session's, and ships no
// recap: it keeps its filesystem, so it is no fresh lineage. §35.5
// instruments it before any decision to widen the recap to it.

// RestoreConversation is how the conversation a restored snapshot was
// minted under compares with the one the session's next prompt names. The
// values are the ones the metric and the logs carry.
type RestoreConversation string

// The outcomes.
const (
	// RestoreConversationHeld: the snapshot was minted under the
	// conversation the prompt names.
	RestoreConversationHeld RestoreConversation = "held"
	// RestoreConversationDiffers: the snapshot was minted under another
	// conversation than the one the prompt names, under one while the
	// prompt names none, or under none while the prompt names one.
	RestoreConversationDiffers RestoreConversation = "differs"
	// RestoreConversationNone: the snapshot was minted under no
	// conversation, and the prompt names none.
	RestoreConversationNone RestoreConversation = "none"
	// RestoreConversationUnknown: nothing was recorded for this snapshot
	// -- it predates the record, or a control plane that does not write it
	// recorded the snapshot.
	RestoreConversationUnknown RestoreConversation = "unknown"
)

// ClassifyRestoreConversation compares the conversation a snapshot was
// minted under with the one the session's next prompt names. recorded is
// false when the sandbox row records no conversation for the snapshot being
// restored; snapshotConversation is then ignored. An empty id is no
// conversation. The first row that matches decides:
//
//  1. not recorded: unknown;
//  2. neither names a conversation: none;
//  3. both name the same one: held;
//  4. otherwise: differs.
func ClassifyRestoreConversation(recorded bool, snapshotConversation, sessionConversation string) RestoreConversation {
	switch {
	case !recorded:
		return RestoreConversationUnknown
	case snapshotConversation == "" && sessionConversation == "":
		return RestoreConversationNone
	case snapshotConversation == sessionConversation:
		return RestoreConversationHeld
	default:
		return RestoreConversationDiffers
	}
}

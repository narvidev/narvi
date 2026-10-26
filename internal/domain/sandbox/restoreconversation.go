package sandbox

// A restore brings back the snapshot's filesystem, the agent runtime's own
// session storage among it, and the prompt the restored gen is sent names
// the conversation the session recorded last (technical plan §3.3, §35.5).
// Nothing checks that the snapshot holds that conversation: the runtime
// adapter echoes the id it is sent. A restore whose snapshot was minted
// under another conversation, or under none, loses the agent's context as
// a fresh lineage does, yet keeps its filesystem, so it is not one, and
// ships no recap. §35.5 instruments that case before any decision to widen
// the recap to it: ClassifyRestoreConversation says which case a restore
// is, and its caller only counts it.

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

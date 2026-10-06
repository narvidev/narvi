package sessionguard

import "fmt"

// Text is what every surface tells a person about r: the REST and MCP 409
// body, the reply on the chat surface, the issue tracker and the code host,
// the session's persisted warning and its one outbox notice. It names the
// spend and the cap, where the cap was set, that the turn in flight was
// left to finish so the spend can pass the cap by that turn's own cost, that
// the recorded figure is a lower bound of the bill, that nothing failed,
// and the remedy.
func Text(r Refusal) string {
	switch r.Reason {
	case ReasonSpendCap:
		return fmt.Sprintf("This session has stopped taking new turns: it has spent %s, at or past its spend cap of %s, set on %s. "+
			"The turn that reached the cap was left to finish, so spending can pass the cap by up to that one turn's own cost, "+
			"and the recorded figure is a lower bound of the bill. Nothing has failed, and the session keeps its sandbox. "+
			"An administrator can raise the cap to let it take turns again.",
			r.Spent, r.Cap, capSourceText(r.Source))
	default:
		return fmt.Sprintf("This session has stopped taking new turns (%s).", r.Reason)
	}
}

// capSourceText names where a cap was set, inside Text's sentence.
func capSourceText(s CapSource) string {
	switch s.Kind {
	case CapSourceAutomation:
		return fmt.Sprintf("the automation %q", s.Name)
	case CapSourceRepo:
		return "repository " + s.Name
	default:
		return "this session"
	}
}

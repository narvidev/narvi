package sessionguard

import "fmt"

// Text is what every surface tells a person about r: the REST body and
// the MCP tool error, the reply on the chat surface, the issue tracker and
// the code host, the session's persisted warning and its one outbox notice,
// and, inside a workflow run's escalation notice, why the run stopped. Every
// sentence holds on every path that emits it: it names the spend and the
// cap and where the cap was set; says that a turn already running when a
// session reaches its cap is left to finish and counted, so the spend shown
// can be past the cap, and that the recorded figure is a lower bound of the
// bill -- without claiming a turn reached the cap, which a cap set below
// the spend already recorded never has; says the session has not failed,
// which the guard never makes it -- a workflow step it stops is the step's
// own record, never the session's; claims nothing about a sandbox the
// session may not have; and gives the remedy.
func Text(r Refusal) string {
	switch r.Reason {
	case ReasonSpendCap:
		return fmt.Sprintf("This session has stopped taking new turns: it has spent %s, at or past its spend cap of %s, set on %s. "+
			"A turn already running when a session reaches its cap is left to finish and its cost is counted, so the spend shown can be past the cap, "+
			"and the recorded figure is a lower bound of the bill. The session itself has not failed. "+
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

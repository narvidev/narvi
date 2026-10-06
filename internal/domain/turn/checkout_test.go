package turn_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/turn"
)

const (
	checkoutHead  = "1111111111111111111111111111111111111111"
	checkoutMoved = "2222222222222222222222222222222222222222"
)

// checkoutBounds are the shipped defaults: 15m, 1m, 10s, 3.
func checkoutBounds() turn.CheckoutBounds {
	return turn.CheckoutBounds{
		Timeout:              15 * time.Minute,
		RefLagWindow:         time.Minute,
		RefetchInterval:      10 * time.Second,
		FailuresBeforeRetire: 3,
	}
}

// requested is a capable gen with one request sent sinceSend ago, its bound
// started sinceRequest ago, answered by reply.
func requested(sinceRequest, sinceSend time.Duration, sends int, reply *turn.CheckoutReply) turn.CheckoutFacts {
	return turn.CheckoutFacts{
		WantSHA:        checkoutHead,
		GenCapable:     true,
		RequestedOnGen: true,
		Reply:          reply,
		SinceRequest:   sinceRequest,
		SinceSend:      sinceSend,
		Sends:          sends,
	}
}

func reply(outcome turn.CheckoutOutcome, head, ref, errText string) *turn.CheckoutReply {
	return &turn.CheckoutReply{Outcome: outcome, HeadSHA: head, RefSHA: ref, Error: errText}
}

// TestDecideReviewCheckout covers every row of technical plan §21.1's
// checkout table (turn.DecideReviewCheckout), each bound from both sides.
func TestDecideReviewCheckout(t *testing.T) {
	t.Parallel()

	b := checkoutBounds()
	checkedOut := reply(turn.CheckoutCheckedOut, checkoutHead, checkoutHead, "")
	tests := []struct {
		name  string
		facts turn.CheckoutFacts
		edit  func(*turn.CheckoutFacts)
		want  turn.CheckoutVerdict
	}{
		// An agent too old to check out.
		{
			name:  "incapable gen with a snapshot is retired",
			facts: turn.CheckoutFacts{WantSHA: checkoutHead, HasSnapshot: true},
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRetireGen, Retirement: turn.CheckoutRetiredOldAgent},
		},
		{
			name:  "incapable gen with no snapshot refuses the turn",
			facts: turn.CheckoutFacts{WantSHA: checkoutHead},
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRefuse, Refusal: turn.CheckoutRefusedUnsupported},
		},
		{
			name:  "incapable gen is never sent a command, even with a request on it",
			facts: requested(time.Second, time.Second, 1, nil),
			edit:  func(f *turn.CheckoutFacts) { f.GenCapable = false },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRefuse, Refusal: turn.CheckoutRefusedUnsupported},
		},
		// No request on the live gen: a first one, or a new gen's.
		{
			name:  "no request on the live gen sends one, looking again at the bound",
			facts: turn.CheckoutFacts{WantSHA: checkoutHead, GenCapable: true, HasSnapshot: true},
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout},
		},
		// No reply.
		{
			name:  "no reply yet waits for the bound",
			facts: requested(4*time.Minute, 4*time.Minute, 1, nil),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: 11 * time.Minute},
		},
		{
			name:  "no reply after a reconnect sends again",
			facts: requested(4*time.Minute, 4*time.Minute, 1, nil),
			edit:  func(f *turn.CheckoutFacts) { f.ReconnectedSinceSend = true },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: 11 * time.Minute},
		},
		{
			name:  "no reply just inside the bound waits",
			facts: requested(b.Timeout-time.Nanosecond, b.Timeout, 1, nil),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: time.Nanosecond},
		},
		{
			name:  "no reply at the bound refuses, a reconnect or not",
			facts: requested(b.Timeout, b.Timeout, 1, nil),
			edit:  func(f *turn.CheckoutFacts) { f.ReconnectedSinceSend = true },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRefuse, Refusal: turn.CheckoutRefusedNoReport},
		},
		// checked_out.
		{
			name:  "checked out at the head, ref at the head, proceeds",
			facts: requested(time.Minute, time.Minute, 1, checkedOut),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutProceed, Outcome: turn.CheckoutCheckedOut, HeadSHA: checkoutHead, RefSHA: checkoutHead},
		},
		{
			name:  "checked out at the head with the ref's tip unknown proceeds",
			facts: requested(time.Minute, time.Minute, 1, reply(turn.CheckoutCheckedOut, checkoutHead, "", "")),
			edit:  func(f *turn.CheckoutFacts) { f.MovedEndsTurn = true },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutProceed, Outcome: turn.CheckoutCheckedOut, HeadSHA: checkoutHead},
		},
		{
			name:  "checked out at the head with the ref moved ends an attempt a lane asks for again",
			facts: requested(time.Minute, time.Minute, 1, reply(turn.CheckoutCheckedOut, checkoutHead, checkoutMoved, "")),
			edit:  func(f *turn.CheckoutFacts) { f.MovedEndsTurn = true },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutEndMoved, Outcome: turn.CheckoutCheckedOut, HeadSHA: checkoutHead, RefSHA: checkoutMoved},
		},
		{
			name:  "checked out at the head with the ref moved runs any other turn on its head",
			facts: requested(time.Minute, time.Minute, 1, reply(turn.CheckoutCheckedOut, checkoutHead, checkoutMoved, "")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutProceed, Outcome: turn.CheckoutCheckedOut, HeadSHA: checkoutHead, RefSHA: checkoutMoved},
		},
		{
			name:  "checked out at the head proceeds past the bound too",
			facts: requested(b.Timeout+time.Minute, b.Timeout, 1, checkedOut),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutProceed, Outcome: turn.CheckoutCheckedOut, HeadSHA: checkoutHead, RefSHA: checkoutHead},
		},
		{
			name:  "checked out at another head reads as failed and is sent again after the interval",
			facts: requested(time.Minute, b.RefetchInterval, 1, reply(turn.CheckoutCheckedOut, checkoutMoved, checkoutMoved, "")),
			want: turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: 14 * time.Minute, AfterFailure: true,
				Outcome: turn.CheckoutFailed, HeadSHA: checkoutMoved, RefSHA: checkoutMoved},
		},
		// sha_absent.
		{
			name:  "sha absent within the lag window waits until the interval",
			facts: requested(5*time.Second, 5*time.Second, 1, reply(turn.CheckoutSHAAbsent, "", checkoutMoved, "")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: 5 * time.Second, Outcome: turn.CheckoutSHAAbsent, RefSHA: checkoutMoved},
		},
		{
			name:  "sha absent within the lag window is fetched again at the interval",
			facts: requested(30*time.Second, b.RefetchInterval, 2, reply(turn.CheckoutSHAAbsent, "", checkoutMoved, "")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout - 30*time.Second, Outcome: turn.CheckoutSHAAbsent, RefSHA: checkoutMoved},
		},
		{
			name:  "sha absent is never counted as a failure, nor spaced by doubling",
			facts: requested(50*time.Second, b.RefetchInterval, 5, reply(turn.CheckoutSHAAbsent, "", checkoutMoved, "")),
			edit:  func(f *turn.CheckoutFacts) { f.Failures = 5 },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout - 50*time.Second, Outcome: turn.CheckoutSHAAbsent, RefSHA: checkoutMoved},
		},
		{
			name:  "sha absent at the lag window ends an attempt a lane asks for again",
			facts: requested(b.RefLagWindow, time.Second, 4, reply(turn.CheckoutSHAAbsent, "", checkoutMoved, "")),
			edit:  func(f *turn.CheckoutFacts) { f.MovedEndsTurn = true },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutEndMoved, Outcome: turn.CheckoutSHAAbsent, RefSHA: checkoutMoved},
		},
		{
			name:  "sha absent at the lag window refuses any other turn, head gone",
			facts: requested(b.RefLagWindow, time.Second, 4, reply(turn.CheckoutSHAAbsent, "", checkoutMoved, "")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRefuse, Refusal: turn.CheckoutRefusedHeadAbsent, Outcome: turn.CheckoutSHAAbsent, RefSHA: checkoutMoved},
		},
		// fetch_failed, busy, failed.
		{
			name:  "fetch failed waits out the interval after the first send",
			facts: requested(3*time.Second, 3*time.Second, 1, reply(turn.CheckoutFetchFailed, "", "", "auth")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: 7 * time.Second, Outcome: turn.CheckoutFetchFailed, Error: "auth"},
		},
		{
			name:  "fetch failed is sent again at the interval, never counted as a failure",
			facts: requested(b.RefetchInterval, b.RefetchInterval, 1, reply(turn.CheckoutFetchFailed, "", "", "auth")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout - b.RefetchInterval, Outcome: turn.CheckoutFetchFailed, Error: "auth"},
		},
		{
			name:  "busy after the third send waits for the interval doubled twice",
			facts: requested(time.Minute, 39*time.Second, 3, reply(turn.CheckoutBusy, "", "", "a turn is running")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: time.Second, Outcome: turn.CheckoutBusy, Error: "a turn is running"},
		},
		{
			name:  "busy after the third send is sent again at the interval doubled twice",
			facts: requested(time.Minute, 40*time.Second, 3, reply(turn.CheckoutBusy, "", "", "a turn is running")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: 14 * time.Minute, Outcome: turn.CheckoutBusy, Error: "a turn is running"},
		},
		{
			name:  "a backoff past the bound waits only until the bound",
			facts: requested(b.Timeout-5*time.Second, time.Second, 7, reply(turn.CheckoutBusy, "", "", "x")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: 5 * time.Second, Outcome: turn.CheckoutBusy, Error: "x"},
		},
		{
			name:  "busy at the bound refuses, naming the error",
			facts: requested(b.Timeout, 5*time.Minute, 6, reply(turn.CheckoutBusy, "", "", "a turn is running")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRefuse, Refusal: turn.CheckoutRefusedError, Outcome: turn.CheckoutBusy, Error: "a turn is running"},
		},
		{
			name:  "a failed reply under the retire count is sent again, counting it",
			facts: requested(10*time.Second, b.RefetchInterval, 1, reply(turn.CheckoutFailed, "", checkoutHead, "index.lock")),
			edit:  func(f *turn.CheckoutFacts) { f.Failures = 0 },
			want: turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout - 10*time.Second, AfterFailure: true,
				Outcome: turn.CheckoutFailed, RefSHA: checkoutHead, Error: "index.lock"},
		},
		{
			name:  "the retire count's failed reply retires the gen",
			facts: requested(30*time.Second, time.Second, 3, reply(turn.CheckoutFailed, "", checkoutHead, "index.lock")),
			edit:  func(f *turn.CheckoutFacts) { f.Failures = 2 },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRetireGen, Retirement: turn.CheckoutRetiredFailing, Outcome: turn.CheckoutFailed, RefSHA: checkoutHead, Error: "index.lock"},
		},
		{
			name:  "a failed reply retires the gen past the bound too, so the turn survives",
			facts: requested(b.Timeout, time.Minute, 3, reply(turn.CheckoutFailed, "", "", "index.lock")),
			edit:  func(f *turn.CheckoutFacts) { f.Failures = 2 },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRetireGen, Retirement: turn.CheckoutRetiredFailing, Outcome: turn.CheckoutFailed, Error: "index.lock"},
		},
		{
			name:  "a turn that retired a gen already retires no other",
			facts: requested(30*time.Second, 20*time.Second, 3, reply(turn.CheckoutFailed, "", "", "index.lock")),
			edit: func(f *turn.CheckoutFacts) {
				f.Failures = 2
				f.RetiredAGen = true
			},
			want: turn.CheckoutVerdict{Action: turn.CheckoutWait, NextLook: 20 * time.Second, Outcome: turn.CheckoutFailed, Error: "index.lock"},
		},
		{
			name:  "fetch failures never retire a gen",
			facts: requested(30*time.Second, 40*time.Second, 3, reply(turn.CheckoutFetchFailed, "", "", "auth")),
			edit:  func(f *turn.CheckoutFacts) { f.Failures = 2 },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout - 30*time.Second, Outcome: turn.CheckoutFetchFailed, Error: "auth"},
		},
		{
			name:  "an outcome the decision does not know reads as failed",
			facts: requested(10*time.Second, b.RefetchInterval, 1, reply("rewound", "", "", "?")),
			want:  turn.CheckoutVerdict{Action: turn.CheckoutSend, NextLook: b.Timeout - 10*time.Second, AfterFailure: true, Outcome: turn.CheckoutFailed, Error: "?"},
		},
		{
			name:  "failed at the bound with a gen already retired refuses, naming the error",
			facts: requested(b.Timeout, time.Minute, 4, reply(turn.CheckoutFailed, "", "", "index.lock")),
			edit:  func(f *turn.CheckoutFacts) { f.RetiredAGen = true },
			want:  turn.CheckoutVerdict{Action: turn.CheckoutRefuse, Refusal: turn.CheckoutRefusedError, Outcome: turn.CheckoutFailed, Error: "index.lock"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := tt.facts
			if tt.edit != nil {
				tt.edit(&f)
			}
			got := turn.DecideReviewCheckout(f, b)
			if got != tt.want {
				t.Errorf("DecideReviewCheckout(%+v) =\n  %+v\nwant\n  %+v", f, got, tt.want)
			}
			if (got.Action == turn.CheckoutSend || got.Action == turn.CheckoutWait) && (got.NextLook <= 0 || got.NextLook > b.Timeout) {
				t.Errorf("NextLook = %s, want it positive and never past the bound", got.NextLook)
			}
		})
	}
}

// TestDecideReviewCheckout_TheWaitIsNeverDueNow walks a sandbox that
// answers busy at once to every send, through to the bound: each look the
// decision asks for is ahead -- the wait never spins -- every send is
// spaced by the doubling interval, and the bound refuses. A send's reply
// arrives as a sandbox event, whose own evaluation is the next look.
func TestDecideReviewCheckout_TheWaitIsNeverDueNow(t *testing.T) {
	t.Parallel()

	b := checkoutBounds()
	var sinceRequest, sinceSend time.Duration
	sends, looks := 1, 0
	var spacings []time.Duration
	for {
		looks++
		if looks > 1000 {
			t.Fatal("no end after 1000 looks")
		}
		v := turn.DecideReviewCheckout(requested(sinceRequest, sinceSend, sends, reply(turn.CheckoutBusy, "", "", "busy")), b)
		switch v.Action {
		case turn.CheckoutRefuse:
			if sinceRequest < b.Timeout {
				t.Fatalf("refused %s into the bound", sinceRequest)
			}
			want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 320 * time.Second}
			if len(spacings) != len(want) {
				t.Fatalf("sends spaced %v, want %v", spacings, want)
			}
			for i := range want {
				if spacings[i] != want[i] {
					t.Fatalf("sends spaced %v, want %v", spacings, want)
				}
			}
			return
		case turn.CheckoutSend, turn.CheckoutWait:
		default:
			t.Fatalf("unexpected verdict %+v", v)
		}
		if v.NextLook <= 0 {
			t.Fatalf("look %d at %s asks to look again in %s: the wait spins", looks, sinceRequest, v.NextLook)
		}
		if v.Action == turn.CheckoutSend {
			// The reply comes at once, and its evaluation is the next look.
			spacings = append(spacings, sinceSend)
			sends++
			sinceSend = 0
			continue
		}
		sinceRequest += v.NextLook
		sinceSend += v.NextLook
	}
}

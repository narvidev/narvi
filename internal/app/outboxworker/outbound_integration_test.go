//go:build integration

package outboxworker_test

import "github.com/narvidev/narvi/internal/app/ports"

// mustNotifier unwraps a notifier constructor's (notifier, error) pair for
// tests that always hand it a real GitHub outbound config, so the error
// can only be a test bug.
func mustNotifier(n ports.Notifier, err error) ports.Notifier {
	if err != nil {
		panic(err)
	}
	return n
}

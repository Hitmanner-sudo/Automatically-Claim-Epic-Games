// Package runner carries out one claim session: see runner_windows.go. This
// file holds the part of it that does not depend on Windows.
package runner

import (
	"strings"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
)

// maxRestarts is how often, within one session, the launcher is started
// again after it went away in the middle of a claim. (It closes itself when
// it has an update of its own to install, and it can crash: seen on a real
// PC, a second before the order button was to be pressed.)
const maxRestarts = 2

// rounds hands the items to run, and hands it what is left for as long as
// run reports that the launcher went away under one of them: lostAt is then
// the index, in what run was given, of the item it was working on, which is
// taken up afresh together with those behind it. Anything else (-1, or an
// error) ends it. run is told whether another start would still follow, and
// must not report a loss when none would.
func rounds[T any](items []T, restarts int, run func(pending []T, mayRestart bool) (lostAt int, err error)) error {
	pending := items
	for round := 0; ; round++ {
		mayRestart := round < restarts
		lostAt, err := run(pending, mayRestart)
		if err != nil || !mayRestart || lostAt < 0 || lostAt >= len(pending) {
			return err
		}
		pending = pending[lostAt:]
	}
}

// afterAPress reports whether a result speaks of an order button that was
// pressed without Epic's answer being seen (the launcher went away, or the
// run was interrupted, after the press).
func afterAPress(res claim.Result) bool {
	return strings.Contains(res.Detail, "after the order button")
}

// settle is what is written down for an offer when a round comes to the
// result res, given what an earlier start of the launcher in the same
// session left for it (prev, if had). A press whose answer was lost with
// the launcher most likely did place the order; when the next round then
// finds the game owned, "owned" must not be the whole story.
func settle(prev claim.Result, had bool, res claim.Result) claim.Result {
	if had && afterAPress(prev) && res.Outcome == claim.Owned {
		res.Detail += "; at an earlier start of the launcher in this session the order button had been pressed before the launcher went away, so that order most likely went through"
	}
	return res
}

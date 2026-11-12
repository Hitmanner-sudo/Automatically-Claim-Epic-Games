package runner

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
)

func TestALauncherThatGoesAwayIsStartedAgainForWhatIsLeft(t *testing.T) {
	var seen [][]string
	lost := []int{1, 0, -1} // goes away under "b", again under "b", then holds
	err := rounds([]string{"a", "b", "c"}, 2, func(pending []string, mayRestart bool) (int, error) {
		seen = append(seen, append([]string(nil), pending...))
		at := lost[len(seen)-1]
		if at >= 0 && !mayRestart {
			t.Fatalf("round %d was told no further start follows", len(seen))
		}
		return at, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "b", "c"}, {"b", "c"}, {"b", "c"}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("rounds were given %v, want %v", seen, want)
	}
}

func TestTheLauncherIsNotStartedAgainForEver(t *testing.T) {
	n := 0
	var last bool
	err := rounds([]string{"a"}, 2, func(pending []string, mayRestart bool) (int, error) {
		n++
		last = mayRestart
		return 0, nil // goes away every time (and says so even when it should not)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("the launcher was started %d times, want 3 (one start and two more)", n)
	}
	if last {
		t.Fatal("the last round was told another start would follow")
	}
}

func TestAnErrorOrAnOrdinaryEndEndsTheSession(t *testing.T) {
	stop := errors.New("the user asked for the launcher")
	n := 0
	err := rounds([]string{"a", "b"}, 2, func(pending []string, mayRestart bool) (int, error) {
		n++
		return 0, stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("after an error: %d round(s), err %v", n, err)
	}
	n = 0
	err = rounds([]string{"a", "b"}, 2, func(pending []string, mayRestart bool) (int, error) {
		n++
		return -1, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("after an ordinary end: %d round(s), err %v", n, err)
	}
	// An index that names nothing is not a loss.
	n = 0
	_ = rounds([]string{"a"}, 2, func(pending []string, mayRestart bool) (int, error) {
		n++
		return 5, nil
	})
	if n != 1 {
		t.Fatalf("an index beyond the items led to %d rounds", n)
	}
}

func TestAPressLostWithTheLauncherIsNotForgotten(t *testing.T) {
	lost := claim.Result{Outcome: claim.Failed, Detail: "the launcher went away after the order button was pressed, before Epic's answer was seen; the library will be checked next time"}
	if !afterAPress(lost) {
		t.Fatal("a result that speaks of a press is not recognised as one")
	}
	for _, d := range []string{
		"the launcher went away just before the order button was to be pressed; nothing was pressed",
		"the launcher went away while the checkout was being opened; nothing was pressed",
		"the launcher went away during the rehearsal, before the last look; nothing was pressed",
	} {
		if afterAPress(claim.Result{Outcome: claim.Failed, Detail: d}) {
			t.Errorf("%q counts as a press", d)
		}
	}

	// The next round finds the game in the library: the record says that a
	// press went before, whoever says "owned", and draws no conclusion.
	for _, owned := range []string{
		"the store says the account has it already; nothing was pressed",
		"Epic says the account has this already (errors.com.epicgames.ecommerce.orderprocessor.purchase_limit_error)",
	} {
		got := settle(lost, true, claim.Result{Outcome: claim.Owned, Detail: owned})
		if got.Outcome != claim.Owned || !strings.HasPrefix(got.Detail, owned) || !strings.HasSuffix(got.Detail, pressedEarlier) {
			t.Errorf("after a lost press, %q was written down as %v", owned, got)
		}
		if strings.Contains(got.Detail, "went through") {
			t.Errorf("the record concludes what it cannot know: %v", got)
		}
	}
	// So does a later start that loses the launcher again before any press
	// (the last one allowed: its result is what stays).
	notPressed := claim.Result{Outcome: claim.Failed, Detail: "the launcher went away while the checkout was being opened; nothing was pressed"}
	if got := settle(lost, true, notPressed); got.Outcome != claim.Failed || !strings.HasSuffix(got.Detail, pressedEarlier) {
		t.Errorf("a loss without a press wrote over the press: %v", got)
	}
	// The note is not itself taken for a press, nor for Epic's answer.
	if afterAPress(claim.Result{Detail: pressedEarlier}) || strings.Contains(pressedEarlier, "after the press") || strings.Contains(pressedEarlier, "Epic says the account has this already") {
		t.Error("the note reads like a press or like an answer from Epic")
	}
	// A claim, and a result that itself speaks of a press, stand as they are.
	claimed := claim.Result{Outcome: claim.Claimed, Detail: "order confirmed by Epic"}
	if got := settle(lost, true, claimed); got != claimed {
		t.Errorf("a claim after a lost press was changed to %v", got)
	}
	if got := settle(lost, true, lost); got != lost {
		t.Errorf("a second lost press was changed to %v", got)
	}
	// Without an earlier press nothing is added.
	plain := claim.Result{Outcome: claim.Owned, Detail: "the store says the account has it already; nothing was pressed"}
	if got := settle(claim.Result{}, false, plain); got != plain {
		t.Errorf("without an earlier press the result was changed to %v", got)
	}
	if got := settle(notPressed, true, plain); got != plain {
		t.Errorf("after a loss without a press the result was changed to %v", got)
	}
}

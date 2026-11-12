package runner

import (
	"errors"
	"reflect"
	"testing"
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

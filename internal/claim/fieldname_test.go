package claim

import (
	"bytes"
	"math/rand/v2"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestAFieldNameIsJudgedAsTheExpressionJudgedIt(t *testing.T) {
	// The check of a form field's name used to be this regular expression.
	// It was replaced by isFieldName because preparing it cost megabytes at
	// every start of the program; the two must give the same answers.
	// (Prepared here and not for the whole package: see the test below.)
	reFieldNameWas := regexp.MustCompile(`^[\p{L}\p{N}_.\-\[\]:$ ]{1,100}$`)
	cases := []string{
		"", "a", "total", "order[total]", "order.total", "order[items][0][price]", "a b", "x$y", "a:b", "a-b_c",
		"naïve", "名前", "٣", "Ⅷ", "½", "ǅ", "ʰ",
		"a=b", "a&b", "a/b", "a\\b", "a+b", "a%b", "a,b", "a;b", "a\"b", "a'b", "a(b)", "a{b}", "a<b>", "a@b", "a#b", "a!b", "a?b", "a*b", "a|b", "a~b", "a^b", "a`b",
		"a\nb", "a\n", "\na", "a\r", "a\tb", "a\x00b", "\x00",
		"é", "́", "a​b", "a b", "a　b", "🙂", "a🙂", "€", "a€", "�", "a�b",
		"\xff", "a\xffb", "\xc3", "a\xc3", "\xc3\x28", "\xf0\x9f\x99",
		strings.Repeat("a", 99), strings.Repeat("a", 100), strings.Repeat("a", 101), strings.Repeat("a", 1000),
		strings.Repeat("名", 99), strings.Repeat("名", 100), strings.Repeat("名", 101),
		strings.Repeat("a", 100) + "\n", strings.Repeat("a", 99) + "=", strings.Repeat("a", 100) + "=",
		strings.Repeat(" ", 100), strings.Repeat("$", 101), strings.Repeat("\xff", 3),
	}
	// And a great many made up from what is allowed, what is not, and what
	// is not even text.
	pool := []string{"a", "Z", "0", "9", "_", ".", "-", "[", "]", ":", "$", " ", "é", "名", "٣", "Ⅷ", "ʰ",
		"=", "&", "/", "\n", "\t", "\x00", "́", "🙂", "€", "�", "\xff", "\xc3", "\xf0\x9f"}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 40000; i++ {
		n := rng.IntN(106)
		allowedOnly := rng.IntN(3) > 0 // most of them all allowed, so that the length decides
		var b strings.Builder
		for j := 0; j < n; j++ {
			if allowedOnly {
				b.WriteString(pool[rng.IntN(17)])
			} else {
				b.WriteString(pool[rng.IntN(len(pool))])
			}
		}
		cases = append(cases, b.String())
	}
	yes := 0
	for _, c := range cases {
		want := reFieldNameWas.MatchString(c)
		if want {
			yes++
		}
		if got := isFieldName(c); got != want {
			t.Fatalf("isFieldName(%q) = %v, the expression says %v", c, got, want)
		}
	}
	if yes < 1000 || yes > len(cases)-1000 {
		t.Fatalf("the cases are lopsided: %d of %d are names", yes, len(cases))
	}
}

// initBudget is how much this package, or one it imports, may allocate
// while the program starts. (The automated Windows run asks the same of
// the whole program: see "What starting costs" in ci.yml.) The background process starts at every sign-in and lives on
// about a megabyte; what a package prepares "just in case" at start-up is
// paid every time and kept for good. (One regular expression once cost
// four megabytes here.)
const initBudget = 512 << 10

func TestStartingThePackageCostsLittle(t *testing.T) {
	// The Go runtime reports what each package's start-up allocated when
	// asked to (GODEBUG=inittrace=1). This test binary holds the package
	// (and its tests' own package-level values, which therefore have to be
	// small too); it is started once more, with nothing to run.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("could not start the test binary again: %v\n%s", err, out.String())
	}
	// init <package> @<when>, <time> clock, <n> bytes, <m> allocs
	re := regexp.MustCompile(`(?m)^init (\S+) @.*?, (\d+) bytes, (\d+) allocs`)
	found := false
	for _, m := range re.FindAllStringSubmatch(out.String(), -1) {
		if !strings.Contains(m[1], "Automatically-Claim-Epic-Games/") {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		t.Logf("%s: %d bytes in %s allocations at start-up", m[1], n, m[3])
		if strings.HasSuffix(m[1], "/internal/claim") {
			found = true
		}
		if n > initBudget {
			t.Errorf("%s allocates %d bytes at start-up, more than the %d it may: prepare it at first use instead", m[1], n, initBudget)
		}
	}
	if !found {
		t.Fatalf("the runtime did not report this package's start-up:\n%s", out.String())
	}
}

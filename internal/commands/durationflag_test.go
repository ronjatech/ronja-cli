package commands

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// waitFlags are the flags that take a length of time. Every one of them takes a
// bare number as seconds, because that is what an agent types first.
var waitFlags = []string{"timeout", "wait-timeout", "wait-interval"}

// timeLengthFlags is every (command, flag) pair in the tree that takes a length
// of time, so a refactor that drops one, or adds one, is named.
var timeLengthFlags = []string{
	"ronja api --timeout", "ronja api --wait-timeout", "ronja api --wait-interval",
	"ronja app test --timeout",
	"ronja database migrate push --timeout", "ronja database migrate status --timeout", "ronja database sql --timeout",
	"ronja propose wait --timeout",
	"ronja query --timeout",
	"ronja workflow run --timeout", "ronja workflow test --timeout",
}

// eachWaitFlag calls fn for every time-length flag in the whole command tree,
// each on a fresh tree so one Set never leaks into the next.
func eachWaitFlag(t *testing.T, fn func(path, name string, set func(string) (string, error))) {
	t.Helper()
	found := map[string]bool{}
	var walk func(c *cobra.Command, path []int)
	walk = func(c *cobra.Command, path []int) {
		for _, name := range waitFlags {
			if c.Flags().Lookup(name) == nil {
				continue
			}
			found[c.CommandPath()+" --"+name] = true
			fn(c.CommandPath(), name, func(value string) (string, error) {
				fresh := newRootCmd()
				for _, i := range path {
					fresh = fresh.Commands()[i]
				}
				flag := fresh.Flags().Lookup(name)
				if err := flag.Value.Set(value); err != nil {
					return "", err
				}
				return flag.Value.String(), nil
			})
		}
		for i, child := range c.Commands() {
			walk(child, append(append([]int(nil), path...), i))
		}
	}
	walk(newRootCmd(), nil)
	for _, want := range timeLengthFlags {
		if !found[want] {
			t.Errorf("%s is not in the tree", want)
		}
		delete(found, want)
	}
	for extra := range found {
		t.Errorf("%s takes a length of time and is not in timeLengthFlags", extra)
	}
}

func TestEveryTimeoutFlagTakesSecondsOrADuration(t *testing.T) {
	eachWaitFlag(t, func(path, name string, set func(string) (string, error)) {
		for value, want := range map[string]string{
			"600": "10m0s", "10m": "10m0s", "1.5": "1.5s", "0": "0", "-5": "-5s",
		} {
			got, err := set(value)
			if err != nil {
				t.Errorf("%s --%s %s: %v", path, name, value, err)
				continue
			}
			if got != want {
				t.Errorf("%s --%s %s = %s, want %s", path, name, value, got, want)
			}
		}
		// Negative control: not a number and not a duration is refused, naming
		// both forms it could have been.
		_, err := set("abc")
		if err == nil {
			t.Errorf("%s --%s abc was accepted", path, name)
		} else if !strings.Contains(err.Error(), "seconds (600)") || !strings.Contains(err.Error(), "10m") {
			t.Errorf("%s --%s abc: %v — want it to name seconds (600) and a duration (10m)", path, name, err)
		}
	})
}

// The help says what a bare number means, or nobody would know to type one.
func TestEveryTimeoutFlagSaysABareNumberIsSeconds(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, name := range waitFlags {
			if flag := c.Flags().Lookup(name); flag != nil && !strings.Contains(flag.Usage, "a bare number is seconds") {
				t.Errorf("%s --%s: usage %q does not say a bare number is seconds", c.CommandPath(), name, flag.Usage)
			}
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(newRootCmd())
}

// Each command's own checks still apply after parsing: a bare negative is
// still refused, and 0 still means "no deadline".
func TestAPIRefusesABareNegativeTimeout(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "--timeout", "-5", "/api/v2/thing"); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Errorf("--timeout -5 = %v, want the negative refusal", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
	if _, err := runCLI(t, t.TempDir(), "api", "--timeout", "600", "/api/v2/thing"); err != nil {
		t.Errorf("--timeout 600: %v", err)
	}
}

// 2^63 ns is one past the largest time.Duration. Converting it does not fail
// the same way everywhere (amd64 wraps negative, arm64 saturates), so the test
// asserts the refusal rather than the sign.
func TestDurationFlagRefusesTwoToTheSixtyThreeNanoseconds(t *testing.T) {
	s := strconv.FormatFloat(math.Exp2(63)/float64(time.Second), 'g', -1, 64)
	if secs, _ := strconv.ParseFloat(s, 64); secs*float64(time.Second) != math.Exp2(63) {
		t.Fatalf("%s seconds is not exactly 2^63 ns", s)
	}
	var d time.Duration
	err := secondsOrDuration{&d}.Set(s)
	if err == nil || !strings.HasPrefix(err.Error(), "out of range") {
		t.Errorf("Set(%s) = %v (d = %d), want the out-of-range refusal", s, err, d)
	}
}

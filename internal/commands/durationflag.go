package commands

import (
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// secondsOrDuration is a time-length flag value that takes a bare number as
// seconds (600, 1.5) as well as a Go duration (10m, 1h30m). Negative values
// parse, so each command's own check is still the one that refuses them.
type secondsOrDuration struct{ d *time.Duration }

func (v secondsOrDuration) String() string {
	if v.d == nil || *v.d == 0 {
		// "0", not "0s": pflag reads "0" as a zero default and leaves
		// "(default 0s)" off the help line, as it does for a duration flag.
		return "0"
	}
	return v.d.String()
}

func (v secondsOrDuration) Set(s string) error {
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		ns := secs * float64(time.Second)
		// >=: as a float64, MaxInt64 is 2^63, which does not fit a Duration.
		if math.IsNaN(ns) || math.IsInf(ns, 0) || math.Abs(ns) >= math.MaxInt64 {
			return errors.New("out of range — give seconds (600) or a duration with a unit (10m, 1h30m)")
		}
		*v.d = time.Duration(ns)
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return errors.New("not a length of time — give seconds (600) or a duration with a unit (10m, 1h30m)")
	}
	*v.d = d
	return nil
}

func (v secondsOrDuration) Type() string { return "duration" }

// durationFlag defines a time-length flag on cmd that reads a bare number as
// seconds. Every time-length flag in the tree uses it; its test lists them.
func durationFlag(cmd *cobra.Command, p *time.Duration, name string, value time.Duration, usage string) {
	*p = value
	cmd.Flags().Var(secondsOrDuration{p}, name, usage+"; a bare number is seconds (600), or give a unit (10m)")
}

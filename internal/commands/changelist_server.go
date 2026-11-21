package commands

import (
	"regexp"
	"time"
)

// Two server rules the wait copies, pinned to the backend by rchangelist's
// TestCLIProposeWaitMirrorsTheServer, which reads this file.

// changeListStuckAfter is echangelist.StuckAfter: the server fails a list
// still applying this long after its approval.
const changeListStuckAfter = 20 * time.Minute

// replacedBy matches the reason rchangelist writes on a list a newer proposal
// replaced, capturing the newer list's id.
var replacedBy = regexp.MustCompile(`replaced by the newer change list (\S+)`)

package commands

import "github.com/ronjatech/ronja-cli/internal/wfdir"

// stateForKey is the one record on (instance, organization), whichever stack
// it belongs to, or nil when there is none or several.
func stateForKey(s *wfdir.State, key wfdir.InstanceKey) *wfdir.InstanceState {
	i, err := wfdir.Find(s.Instances, wfdir.InstanceBaseline.Key, key)
	if err != nil || i < 0 {
		return nil
	}
	return s.Instances[i].State
}

// stateSetKey records a legacy baseline, keyed by (instance, organization)
// alone, as every CLI before per-stack records wrote it.
func stateSetKey(s *wfdir.State, key wfdir.InstanceKey, inst *wfdir.InstanceState) {
	s.Set(wfdir.BaselineKey{Key: key}, inst)
}

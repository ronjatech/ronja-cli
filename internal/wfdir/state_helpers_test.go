package wfdir

// stateForKey is the one record on (instance, organization), whichever stack
// it belongs to, or nil when there is none or several.
func stateForKey(s *State, key InstanceKey) *InstanceState {
	i, err := Find(s.Instances, InstanceBaseline.Key, key)
	if err != nil || i < 0 {
		return nil
	}
	return s.Instances[i].State
}

// stateSetKey records a legacy baseline, keyed by (instance, organization)
// alone, as every CLI before per-stack records wrote it.
func stateSetKey(s *State, key InstanceKey, inst *InstanceState) { s.Set(BaselineKey{Key: key}, inst) }

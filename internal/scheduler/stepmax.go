package scheduler

import "time"

// LongestStep is the longest time one event took in the core loop since the
// start, with the event's name: the bound of every stall a read or a
// heartbeat can meet. Readers outside the package (the availability
// sampler's tests) measure with it.
func (s *Scheduler) LongestStep() (time.Duration, string) {
	name, _ := s.stepMaxEv.Load().(string)
	return time.Duration(s.stepMax.Load()), name
}

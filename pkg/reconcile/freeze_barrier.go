package reconcile

import "sync"

type FreezeBarrier struct{ mutex sync.RWMutex }

func (barrier *FreezeBarrier) Enter() func() {
	if barrier == nil {
		return func() {}
	}
	barrier.mutex.RLock()
	return barrier.mutex.RUnlock
}

func (barrier *FreezeBarrier) Drain() func() {
	if barrier == nil {
		return func() {}
	}
	barrier.mutex.Lock()
	return barrier.mutex.Unlock
}

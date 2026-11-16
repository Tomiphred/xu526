package platform

import "time"

// autoscaleLoop runs the scaling decision for every app once per tick.
//
// The signal is request concurrency, measured by the router itself:
//
//	desired = ceil(peak in-flight requests / target per instance)
//
// Scale-up is immediate (a new peak enters the window at once). Scale-down
// uses the highest peak over the last ScaleDownSec seconds, so short dips
// don't cause churn. Below one instance, the app goes to zero only after
// IdleTimeoutSec with no requests at all.
func (p *Platform) autoscaleLoop() {
	t := time.NewTicker(p.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case now := <-t.C:
			p.mu.RLock()
			apps := make([]*app, 0, len(p.apps))
			for _, a := range p.apps {
				apps = append(apps, a)
			}
			p.mu.RUnlock()
			need := p.neededApps(apps)
			for _, a := range apps {
				n := a.desired(now, p.cfg.Tick)
				if need[a.spec.Name] && n == 0 {
					n = min(1, a.spec.MaxScale) // something running depends on it
				}
				a.scaleTo(n)
			}
		}
	}
}

func (a *app) desired(now time.Time, tick time.Duration) int {
	cur := a.inflight.Load()
	peak := max(a.peak.Swap(cur), cur)

	size := max(1, int(time.Duration(a.spec.ScaleDownSec)*time.Second/tick))
	a.window = append(a.window, peak)
	if len(a.window) > size {
		a.window = a.window[len(a.window)-size:]
	}
	var top int64
	for _, v := range a.window {
		top = max(top, v)
	}

	target := int64(a.spec.Target)
	want := int((top + target - 1) / target)

	if want == 0 {
		idle := now.Sub(time.Unix(0, a.lastReq.Load()))
		if idle < time.Duration(a.spec.IdleTimeoutSec)*time.Second {
			a.mu.Lock()
			running := a.countLocked()
			a.mu.Unlock()
			want = min(1, running) // keep one warm until the idle timeout
		}
	}
	return min(max(want, a.spec.MinScale), a.spec.MaxScale)
}

package outboundgroup

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Dreamacro/clash/adapter/outbound"
	"github.com/Dreamacro/clash/common/atomic"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/constant/provider"
	types "github.com/Dreamacro/clash/constant/provider"
	"github.com/Dreamacro/clash/log"
	"github.com/Dreamacro/clash/tunnel"

	"github.com/dlclark/regexp2"
)

type GroupBase struct {
	*outbound.Base
	filterRegs       []*regexp2.Regexp
	excludeFilterReg *regexp2.Regexp
	excludeTypeArray []string
	providers        []provider.ProxyProvider
	failedTestMux    sync.Mutex
	failedTimes      int
	failedTime       time.Time
	failedTesting    *atomic.Bool
	proxies          [][]C.Proxy
	versions         []atomic.Uint32
}

type GroupBaseOption struct {
	outbound.BaseOption
	filter        string
	excludeFilter string
	excludeType   string
	providers     []provider.ProxyProvider
}

func NewGroupBase(opt GroupBaseOption) *GroupBase {
	var excludeFilterReg *regexp2.Regexp
	if opt.excludeFilter != "" {
		excludeFilterReg = regexp2.MustCompile(opt.excludeFilter, 0)
	}
	var excludeTypeArray []string
	if opt.excludeType != "" {
		excludeTypeArray = strings.Split(opt.excludeType, "|")
	}

	var filterRegs []*regexp2.Regexp
	if opt.filter != "" {
		for _, filter := range strings.Split(opt.filter, "`") {
			filterReg := regexp2.MustCompile(filter, 0)
			filterRegs = append(filterRegs, filterReg)
		}
	}

	gb := &GroupBase{
		Base:             outbound.NewBase(opt.BaseOption),
		filterRegs:       filterRegs,
		excludeFilterReg: excludeFilterReg,
		excludeTypeArray: excludeTypeArray,
		providers:        opt.providers,
		failedTesting:    atomic.NewBool(false),
	}

	gb.proxies = make([][]C.Proxy, len(opt.providers))
	gb.versions = make([]atomic.Uint32, len(opt.providers))

	return gb
}

func (gb *GroupBase) Touch() {
	for _, pd := range gb.providers {
		pd.Touch()
	}
}

func (gb *GroupBase) GetProxies(touch bool) []C.Proxy {
	var proxies []C.Proxy
	if len(gb.filterRegs) == 0 {
		for _, pd := range gb.providers {
			if touch {
				pd.Touch()
			}
			proxies = append(proxies, pd.Proxies()...)
		}
	} else {
		for i, pd := range gb.providers {
			if touch {
				pd.Touch()
			}

			if pd.VehicleType() == types.Compatible {
				gb.versions[i].Store(pd.Version())
				gb.proxies[i] = pd.Proxies()
				continue
			}

			version := gb.versions[i].Load()
			if version != pd.Version() && gb.versions[i].CompareAndSwap(version, pd.Version()) {
				var (
					proxies    []C.Proxy
					newProxies []C.Proxy
				)

				proxies = pd.Proxies()
				proxiesSet := map[string]struct{}{}
				for _, filterReg := range gb.filterRegs {
					for _, p := range proxies {
						name := p.Name()
						if mat, _ := filterReg.FindStringMatch(name); mat != nil {
							if _, ok := proxiesSet[name]; !ok {
								proxiesSet[name] = struct{}{}
								newProxies = append(newProxies, p)
							}
						}
					}
				}

				gb.proxies[i] = newProxies
			}
		}

		for _, p := range gb.proxies {
			proxies = append(proxies, p...)
		}
	}

	if len(gb.providers) > 1 && len(gb.filterRegs) > 1 {
		var newProxies []C.Proxy
		proxiesSet := map[string]struct{}{}
		for _, filterReg := range gb.filterRegs {
			for _, p := range proxies {
				name := p.Name()
				if mat, _ := filterReg.FindStringMatch(name); mat != nil {
					if _, ok := proxiesSet[name]; !ok {
						proxiesSet[name] = struct{}{}
						newProxies = append(newProxies, p)
					}
				}
			}
		}
		for _, p := range proxies { // add not matched proxies at the end
			name := p.Name()
			if _, ok := proxiesSet[name]; !ok {
				proxiesSet[name] = struct{}{}
				newProxies = append(newProxies, p)
			}
		}
		proxies = newProxies
	}
	if gb.excludeTypeArray != nil {
		var newProxies []C.Proxy
		for _, p := range proxies {
			mType := p.Type().String()
			flag := false
			for i := range gb.excludeTypeArray {
				if strings.EqualFold(mType, gb.excludeTypeArray[i]) {
					flag = true
					break
				}

			}
			if flag {
				continue
			}
			newProxies = append(newProxies, p)
		}
		proxies = newProxies
	}

	if gb.excludeFilterReg != nil {
		var newProxies []C.Proxy
		for _, p := range proxies {
			name := p.Name()
			if mat, _ := gb.excludeFilterReg.FindStringMatch(name); mat != nil {
				continue
			}
			newProxies = append(newProxies, p)
		}
		proxies = newProxies
	}

	if len(proxies) == 0 {
		return append(proxies, tunnel.Proxies()["COMPATIBLE"])
	}

	return proxies
}

func (gb *GroupBase) URLTest(ctx context.Context, url string) (map[string]uint16, error) {
	var wg sync.WaitGroup
	var lock sync.Mutex
	mp := map[string]uint16{}
	proxies := gb.GetProxies(false)
	for _, proxy := range proxies {
		proxy := proxy
		wg.Add(1)
		go func() {
			delay, err := proxy.URLTest(ctx, url)
			if err == nil {
				lock.Lock()
				mp[proxy.Name()] = delay
				lock.Unlock()
			}

			wg.Done()
		}()
	}
	wg.Wait()

	if len(mp) == 0 {
		return mp, fmt.Errorf("get delay: all proxies timeout")
	} else {
		return mp, nil
	}
}

func (gb *GroupBase) onDialFailed(adapterType C.AdapterType, err error) {
	if adapterType == C.Direct || adapterType == C.Compatible || adapterType == C.Reject || adapterType == C.Pass {
		return
	}

	if strings.Contains(err.Error(), "connection refused") {
		go gb.healthCheck()
		return
	}

	go func() {
		gb.failedTestMux.Lock()
		defer gb.failedTestMux.Unlock()

		gb.failedTimes++
		if gb.failedTimes == 1 {
			log.Debugln("ProxyGroup: %s first failed", gb.Name())
			gb.failedTime = time.Now()
		} else {
			if time.Since(gb.failedTime) > gb.failedTimeoutInterval() {
				gb.failedTimes = 0
				return
			}

			log.Debugln("ProxyGroup: %s failed count: %d", gb.Name(), gb.failedTimes)
			if gb.failedTimes >= gb.maxFailedTimes() {
				log.Warnln("because %s failed multiple times, active health check", gb.Name())
				gb.healthCheck()
			}
		}
	}()
}

func (gb *GroupBase) healthCheck() {
	if gb.failedTesting.Load() {
		return
	}

	gb.failedTesting.Store(true)
	wg := sync.WaitGroup{}
	for _, proxyProvider := range gb.providers {
		wg.Add(1)
		proxyProvider := proxyProvider
		go func() {
			defer wg.Done()
			proxyProvider.HealthCheck()
		}()
	}

	wg.Wait()
	gb.failedTesting.Store(false)
	gb.failedTimes = 0
}

func (gb *GroupBase) failedIntervalTime() int64 {
	return 5 * time.Second.Milliseconds()
}

func (gb *GroupBase) onDialSuccess() {
	if !gb.failedTesting.Load() {
		gb.failedTimes = 0
	}
}

func (gb *GroupBase) maxFailedTimes() int {
	return 5
}

func (gb *GroupBase) failedTimeoutInterval() time.Duration {
	return 5 * time.Second
}

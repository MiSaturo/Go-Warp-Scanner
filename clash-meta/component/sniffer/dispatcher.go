package sniffer

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/Dreamacro/clash/common/cache"
	N "github.com/Dreamacro/clash/common/net"
	"github.com/Dreamacro/clash/component/trie"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/constant/sniffer"
	"github.com/Dreamacro/clash/log"
)

var (
	ErrorUnsupportedSniffer = errors.New("unsupported sniffer")
	ErrorSniffFailed        = errors.New("all sniffer failed")
	ErrNoClue               = errors.New("not enough information for making a decision")
)

var Dispatcher *SnifferDispatcher

type SnifferDispatcher struct {
	enable           bool
	sniffers         map[sniffer.Sniffer]SnifferConfig
	forceDomain      *trie.DomainSet
	skipSNI          *trie.DomainSet
	skipList         *cache.LruCache[string, uint8]
	rwMux            sync.RWMutex
	forceDnsMapping  bool
	parsePureIp      bool
}

func (sd *SnifferDispatcher) TCPSniff(conn *N.BufferedConn, metadata *C.Metadata) {
	if (metadata.Host == "" && sd.parsePureIp) || sd.forceDomain.Has(metadata.Host) || (metadata.DNSMode == C.DNSMapping && sd.forceDnsMapping) {
		port, err := strconv.ParseUint(metadata.DstPort, 10, 16)
		if err != nil {
			log.Debugln("[Sniffer] Dst port is error")
			return
		}

		inWhitelist := false
		overrideDest := false
		for sniffer, config := range sd.sniffers {
			if sniffer.SupportNetwork() == C.TCP || sniffer.SupportNetwork() == C.ALLNet {
				inWhitelist = sniffer.SupportPort(uint16(port))
				if inWhitelist {
					overrideDest = config.OverrideDest
					break
				}
			}
		}

		if !inWhitelist {
			return
		}

		sd.rwMux.RLock()
		dst := fmt.Sprintf("%s:%s", metadata.DstIP, metadata.DstPort)
		if count, ok := sd.skipList.Get(dst); ok && count > 5 {
			log.Debugln("[Sniffer] Skip sniffing[%s] due to multiple failures", dst)
			defer sd.rwMux.RUnlock()
			return
		}
		sd.rwMux.RUnlock()

		if host, err := sd.sniffDomain(conn, metadata); err != nil {
			sd.cacheSniffFailed(metadata)
			log.Debugln("[Sniffer] All sniffing sniff failed with from [%s:%s] to [%s:%s]", metadata.SrcIP, metadata.SrcPort, metadata.String(), metadata.DstPort)
			return
		} else {
			if sd.skipSNI.Has(host) {
				log.Debugln("[Sniffer] Skip sni[%s]", host)
				return
			}

			sd.rwMux.RLock()
			sd.skipList.Delete(dst)
			sd.rwMux.RUnlock()

			sd.replaceDomain(metadata, host, overrideDest)
		}
	}
}

func (sd *SnifferDispatcher) replaceDomain(metadata *C.Metadata, host string, overrideDest bool) {
	metadata.SniffHost = host
	if overrideDest {
		metadata.Host = host
	}
	metadata.DNSMode = C.DNSNormal
	log.Debugln("[Sniffer] Sniff TCP [%s]-->[%s] success, replace domain [%s]-->[%s]",
		metadata.SourceDetail(),
		metadata.RemoteAddress(),
		metadata.Host, host)
}

func (sd *SnifferDispatcher) Enable() bool {
	return sd.enable
}

func (sd *SnifferDispatcher) sniffDomain(conn *N.BufferedConn, metadata *C.Metadata) (string, error) {
	for s := range sd.sniffers {
		if s.SupportNetwork() == C.TCP {
			_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			_, err := conn.Peek(1)
			_ = conn.SetReadDeadline(time.Time{})
			if err != nil {
				_, ok := err.(*net.OpError)
				if ok {
					sd.cacheSniffFailed(metadata)
					log.Errorln("[Sniffer] [%s] may not have any sent data, Consider adding skip", metadata.DstIP.String())
					_ = conn.Close()
				}

				return "", err
			}

			bufferedLen := conn.Buffered()
			bytes, err := conn.Peek(bufferedLen)
			if err != nil {
				log.Debugln("[Sniffer] the data length not enough")
				continue
			}

			host, err := s.SniffTCP(bytes)
			if err != nil {
				//log.Debugln("[Sniffer] [%s] Sniff data failed %s", s.Protocol(), metadata.DstIP)
				continue
			}

			_, err = netip.ParseAddr(host)
			if err == nil {
				//log.Debugln("[Sniffer] [%s] Sniff data failed %s", s.Protocol(), metadata.DstIP)
				continue
			}

			return host, nil
		}
	}

	return "", ErrorSniffFailed
}

func (sd *SnifferDispatcher) cacheSniffFailed(metadata *C.Metadata) {
	sd.rwMux.Lock()
	dst := fmt.Sprintf("%s:%s", metadata.DstIP, metadata.DstPort)
	count, _ := sd.skipList.Get(dst)
	if count <= 5 {
		count++
	}
	sd.skipList.Set(dst, count)
	sd.rwMux.Unlock()
}

func NewCloseSnifferDispatcher() (*SnifferDispatcher, error) {
	dispatcher := SnifferDispatcher{
		enable: false,
	}

	return &dispatcher, nil
}

func NewSnifferDispatcher(snifferConfig map[sniffer.Type]SnifferConfig,
	forceDomain *trie.DomainSet, skipSNI *trie.DomainSet,
	forceDnsMapping bool, parsePureIp bool) (*SnifferDispatcher, error) {
	dispatcher := SnifferDispatcher{
		enable:          true,
		forceDomain:     forceDomain,
		skipSNI:         skipSNI,
		skipList:        cache.New(cache.WithSize[string, uint8](128), cache.WithAge[string, uint8](600)),
		forceDnsMapping: forceDnsMapping,
		parsePureIp:     parsePureIp,
		sniffers:        make(map[sniffer.Sniffer]SnifferConfig, 0),
	}

	for snifferName, config := range snifferConfig {
		s, err := NewSniffer(snifferName, config)
		if err != nil {
			log.Errorln("Sniffer name[%s] is error", snifferName)
			return &SnifferDispatcher{enable: false}, err
		}
		dispatcher.sniffers[s] = config
	}

	return &dispatcher, nil
}

func NewSniffer(name sniffer.Type, snifferConfig SnifferConfig) (sniffer.Sniffer, error) {
	switch name {
	case sniffer.TLS:
		return NewTLSSniffer(snifferConfig)
	case sniffer.HTTP:
		return NewHTTPSniffer(snifferConfig)
	default:
		return nil, ErrorUnsupportedSniffer
	}
}

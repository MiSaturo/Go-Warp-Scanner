package outbound

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/Dreamacro/clash/common/convert"
	N "github.com/Dreamacro/clash/common/net"
	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/component/proxydialer"
	"github.com/Dreamacro/clash/component/resolver"
	tlsC "github.com/Dreamacro/clash/component/tls"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"
	"github.com/Dreamacro/clash/transport/gun"
	"github.com/Dreamacro/clash/transport/socks5"
	"github.com/Dreamacro/clash/transport/vless"
	"github.com/Dreamacro/clash/transport/vmess"

	vmessSing "github.com/sagernet/sing-vmess"
	"github.com/sagernet/sing-vmess/packetaddr"
	M "github.com/sagernet/sing/common/metadata"
)

const (
	// max packet length
	maxLength = 1024 << 3
)

type Vless struct {
	*Base
	client *vless.Client
	option *VlessOption

	// for gun mux
	gunTLSConfig *tls.Config
	gunConfig    *gun.Config
	transport    *gun.TransportWrap

	realityConfig *tlsC.RealityConfig
}

type VlessOption struct {
	BasicOption
	Name              string            `proxy:"name"`
	Server            string            `proxy:"server"`
	Port              int               `proxy:"port"`
	UUID              string            `proxy:"uuid"`
	Flow              string            `proxy:"flow,omitempty"`
	FlowShow          bool              `proxy:"flow-show,omitempty"`
	TLS               bool              `proxy:"tls,omitempty"`
	UDP               bool              `proxy:"udp,omitempty"`
	PacketAddr        bool              `proxy:"packet-addr,omitempty"`
	XUDP              bool              `proxy:"xudp,omitempty"`
	PacketEncoding    string            `proxy:"packet-encoding,omitempty"`
	Network           string            `proxy:"network,omitempty"`
	RealityOpts       RealityOptions    `proxy:"reality-opts,omitempty"`
	HTTPOpts          HTTPOptions       `proxy:"http-opts,omitempty"`
	HTTP2Opts         HTTP2Options      `proxy:"h2-opts,omitempty"`
	GrpcOpts          GrpcOptions       `proxy:"grpc-opts,omitempty"`
	WSOpts            WSOptions         `proxy:"ws-opts,omitempty"`
	WSPath            string            `proxy:"ws-path,omitempty"`
	WSHeaders         map[string]string `proxy:"ws-headers,omitempty"`
	SkipCertVerify    bool              `proxy:"skip-cert-verify,omitempty"`
	Fingerprint       string            `proxy:"fingerprint,omitempty"`
	ServerName        string            `proxy:"servername,omitempty"`
	ClientFingerprint string            `proxy:"client-fingerprint,omitempty"`
}

func (v *Vless) StreamConnContext(ctx context.Context, c net.Conn, metadata *C.Metadata) (net.Conn, error) {
	var err error

	if tlsC.HaveGlobalFingerprint() && len(v.option.ClientFingerprint) == 0 {
		v.option.ClientFingerprint = tlsC.GetGlobalFingerprint()
	}

	switch v.option.Network {
	case "ws":
		host, port, _ := net.SplitHostPort(v.addr)
		wsOpts := &vmess.WebsocketConfig{
			Host:                host,
			Port:                port,
			Path:                v.option.WSOpts.Path,
			MaxEarlyData:        v.option.WSOpts.MaxEarlyData,
			EarlyDataHeaderName: v.option.WSOpts.EarlyDataHeaderName,
			ClientFingerprint:   v.option.ClientFingerprint,
			Headers:             http.Header{},
		}

		if len(v.option.WSOpts.Headers) != 0 {
			for key, value := range v.option.WSOpts.Headers {
				wsOpts.Headers.Add(key, value)
			}
		}
		if v.option.TLS {
			wsOpts.TLS = true
			tlsConfig := &tls.Config{
				MinVersion:         tls.VersionTLS12,
				ServerName:         host,
				InsecureSkipVerify: v.option.SkipCertVerify,
				NextProtos:         []string{"http/1.1"},
			}

			if len(v.option.Fingerprint) == 0 {
				wsOpts.TLSConfig = tlsC.GetGlobalTLSConfig(tlsConfig)
			} else {
				wsOpts.TLSConfig, err = tlsC.GetSpecifiedFingerprintTLSConfig(tlsConfig, v.option.Fingerprint)
				if err != nil {
					return nil, err
				}
			}

			if v.option.ServerName != "" {
				wsOpts.TLSConfig.ServerName = v.option.ServerName
			} else if host := wsOpts.Headers.Get("Host"); host != "" {
				wsOpts.TLSConfig.ServerName = host
			}
		} else {
			if host := wsOpts.Headers.Get("Host"); host == "" {
				wsOpts.Headers.Set("Host", convert.RandHost())
				convert.SetUserAgent(wsOpts.Headers)
			}
		}
		c, err = vmess.StreamWebsocketConn(ctx, c, wsOpts)
	case "http":
		// readability first, so just copy default TLS logic
		c, err = v.streamTLSOrXTLSConn(ctx, c, false)
		if err != nil {
			return nil, err
		}

		host, _, _ := net.SplitHostPort(v.addr)
		httpOpts := &vmess.HTTPConfig{
			Host:    host,
			Method:  v.option.HTTPOpts.Method,
			Path:    v.option.HTTPOpts.Path,
			Headers: v.option.HTTPOpts.Headers,
		}

		c = vmess.StreamHTTPConn(c, httpOpts)
	case "h2":
		c, err = v.streamTLSOrXTLSConn(ctx, c, true)
		if err != nil {
			return nil, err
		}

		h2Opts := &vmess.H2Config{
			Hosts: v.option.HTTP2Opts.Host,
			Path:  v.option.HTTP2Opts.Path,
		}

		c, err = vmess.StreamH2Conn(c, h2Opts)
	case "grpc":
		c, err = gun.StreamGunWithConn(c, v.gunTLSConfig, v.gunConfig, v.realityConfig)
	default:
		// default tcp network
		// handle TLS And XTLS
		c, err = v.streamTLSOrXTLSConn(ctx, c, false)
	}

	if err != nil {
		return nil, err
	}

	return v.streamConn(c, metadata)
}

func (v *Vless) streamConn(c net.Conn, metadata *C.Metadata) (conn net.Conn, err error) {
	if metadata.NetWork == C.UDP {
		if v.option.PacketAddr {
			metadata = &C.Metadata{
				NetWork: C.UDP,
				Host:    packetaddr.SeqPacketMagicAddress,
				DstPort: "443",
			}
		} else {
			metadata = &C.Metadata{ // a clear metadata only contains ip
				NetWork: C.UDP,
				DstIP:   metadata.DstIP,
				DstPort: metadata.DstPort,
			}
		}
		conn, err = v.client.StreamConn(c, parseVlessAddr(metadata, v.option.XUDP))
		if v.option.PacketAddr {
			conn = packetaddr.NewBindConn(conn)
		}
	} else {
		conn, err = v.client.StreamConn(c, parseVlessAddr(metadata, false))
	}
	if err != nil {
		conn = nil
	}
	return
}

func (v *Vless) streamTLSOrXTLSConn(ctx context.Context, conn net.Conn, isH2 bool) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(v.addr)

	if v.isLegacyXTLSEnabled() && !isH2 {
		xtlsOpts := vless.XTLSConfig{
			Host:           host,
			SkipCertVerify: v.option.SkipCertVerify,
			Fingerprint:    v.option.Fingerprint,
		}

		if v.option.ServerName != "" {
			xtlsOpts.Host = v.option.ServerName
		}

		return vless.StreamXTLSConn(ctx, conn, &xtlsOpts)

	} else if v.option.TLS {
		tlsOpts := vmess.TLSConfig{
			Host:              host,
			SkipCertVerify:    v.option.SkipCertVerify,
			FingerPrint:       v.option.Fingerprint,
			ClientFingerprint: v.option.ClientFingerprint,
			Reality:           v.realityConfig,
		}

		if isH2 {
			tlsOpts.NextProtos = []string{"h2"}
		}

		if v.option.ServerName != "" {
			tlsOpts.Host = v.option.ServerName
		}

		return vmess.StreamTLSConn(ctx, conn, &tlsOpts)
	}

	return conn, nil
}

func (v *Vless) isLegacyXTLSEnabled() bool {
	return v.client.Addons != nil && v.client.Addons.Flow != vless.XRV
}

// DialContext implements C.ProxyAdapter
func (v *Vless) DialContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (_ C.Conn, err error) {
	// gun transport
	if v.transport != nil && len(opts) == 0 {
		c, err := gun.StreamGunWithTransport(v.transport, v.gunConfig)
		if err != nil {
			return nil, err
		}
		defer func(c net.Conn) {
			safeConnClose(c, err)
		}(c)

		c, err = v.client.StreamConn(c, parseVlessAddr(metadata, v.option.XUDP))
		if err != nil {
			return nil, err
		}

		return NewConn(c, v), nil
	}
	return v.DialContextWithDialer(ctx, dialer.NewDialer(v.Base.DialOptions(opts...)...), metadata)
}

// DialContextWithDialer implements C.ProxyAdapter
func (v *Vless) DialContextWithDialer(ctx context.Context, dialer C.Dialer, metadata *C.Metadata) (_ C.Conn, err error) {
	if len(v.option.DialerProxy) > 0 {
		dialer, err = proxydialer.NewByName(v.option.DialerProxy, dialer)
		if err != nil {
			return nil, err
		}
	}
	c, err := dialer.DialContext(ctx, "tcp", v.addr)
	if err != nil {
		return nil, fmt.Errorf("%s connect error: %s", v.addr, err.Error())
	}
	tcpKeepAlive(c)
	defer func(c net.Conn) {
		safeConnClose(c, err)
	}(c)

	c, err = v.StreamConnContext(ctx, c, metadata)
	if err != nil {
		return nil, fmt.Errorf("%s connect error: %s", v.addr, err.Error())
	}
	return NewConn(c, v), err
}

// ListenPacketContext implements C.ProxyAdapter
func (v *Vless) ListenPacketContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (_ C.PacketConn, err error) {
	// vless use stream-oriented udp with a special address, so we need a net.UDPAddr
	if !metadata.Resolved() {
		ip, err := resolver.ResolveIP(ctx, metadata.Host)
		if err != nil {
			return nil, errors.New("can't resolve ip")
		}
		metadata.DstIP = ip
	}
	var c net.Conn
	// gun transport
	if v.transport != nil && len(opts) == 0 {
		c, err = gun.StreamGunWithTransport(v.transport, v.gunConfig)
		if err != nil {
			return nil, err
		}
		defer func(c net.Conn) {
			safeConnClose(c, err)
		}(c)

		c, err = v.streamConn(c, metadata)
		if err != nil {
			return nil, fmt.Errorf("new vless client error: %v", err)
		}

		return v.ListenPacketOnStreamConn(ctx, c, metadata)
	}
	return v.ListenPacketWithDialer(ctx, dialer.NewDialer(v.Base.DialOptions(opts...)...), metadata)
}

// ListenPacketWithDialer implements C.ProxyAdapter
func (v *Vless) ListenPacketWithDialer(ctx context.Context, dialer C.Dialer, metadata *C.Metadata) (_ C.PacketConn, err error) {
	if len(v.option.DialerProxy) > 0 {
		dialer, err = proxydialer.NewByName(v.option.DialerProxy, dialer)
		if err != nil {
			return nil, err
		}
	}

	// vless use stream-oriented udp with a special address, so we need a net.UDPAddr
	if !metadata.Resolved() {
		ip, err := resolver.ResolveIP(ctx, metadata.Host)
		if err != nil {
			return nil, errors.New("can't resolve ip")
		}
		metadata.DstIP = ip
	}

	c, err := dialer.DialContext(ctx, "tcp", v.addr)
	if err != nil {
		return nil, fmt.Errorf("%s connect error: %s", v.addr, err.Error())
	}
	tcpKeepAlive(c)
	defer func(c net.Conn) {
		safeConnClose(c, err)
	}(c)

	c, err = v.StreamConnContext(ctx, c, metadata)
	if err != nil {
		return nil, fmt.Errorf("new vless client error: %v", err)
	}

	return v.ListenPacketOnStreamConn(ctx, c, metadata)
}

// SupportWithDialer implements C.ProxyAdapter
func (v *Vless) SupportWithDialer() C.NetWork {
	return C.ALLNet
}

// ListenPacketOnStreamConn implements C.ProxyAdapter
func (v *Vless) ListenPacketOnStreamConn(ctx context.Context, c net.Conn, metadata *C.Metadata) (_ C.PacketConn, err error) {
	// vless use stream-oriented udp with a special address, so we need a net.UDPAddr
	if !metadata.Resolved() {
		ip, err := resolver.ResolveIP(ctx, metadata.Host)
		if err != nil {
			return nil, errors.New("can't resolve ip")
		}
		metadata.DstIP = ip
	}

	if v.option.XUDP {
		return newPacketConn(N.NewThreadSafePacketConn(
			vmessSing.NewXUDPConn(c, M.SocksaddrFromNet(metadata.UDPAddr())),
		), v), nil
	} else if v.option.PacketAddr {
		return newPacketConn(N.NewThreadSafePacketConn(
			packetaddr.NewConn(&vlessPacketConn{
				Conn: c, rAddr: metadata.UDPAddr(),
			}, M.SocksaddrFromNet(metadata.UDPAddr())),
		), v), nil
	}
	return newPacketConn(&vlessPacketConn{Conn: c, rAddr: metadata.UDPAddr()}, v), nil
}

// SupportUOT implements C.ProxyAdapter
func (v *Vless) SupportUOT() bool {
	return true
}

func parseVlessAddr(metadata *C.Metadata, xudp bool) *vless.DstAddr {
	var addrType byte
	var addr []byte
	switch metadata.AddrType() {
	case socks5.AtypIPv4:
		addrType = vless.AtypIPv4
		addr = make([]byte, net.IPv4len)
		copy(addr[:], metadata.DstIP.AsSlice())
	case socks5.AtypIPv6:
		addrType = vless.AtypIPv6
		addr = make([]byte, net.IPv6len)
		copy(addr[:], metadata.DstIP.AsSlice())
	case socks5.AtypDomainName:
		addrType = vless.AtypDomainName
		addr = make([]byte, len(metadata.Host)+1)
		addr[0] = byte(len(metadata.Host))
		copy(addr[1:], metadata.Host)
	}

	port, _ := strconv.ParseUint(metadata.DstPort, 10, 16)
	return &vless.DstAddr{
		UDP:      metadata.NetWork == C.UDP,
		AddrType: addrType,
		Addr:     addr,
		Port:     uint16(port),
		Mux:      metadata.NetWork == C.UDP && xudp,
	}
}

type vlessPacketConn struct {
	net.Conn
	rAddr  net.Addr
	remain int
	mux    sync.Mutex
	cache  [2]byte
}

func (c *vlessPacketConn) writePacket(payload []byte) (int, error) {
	binary.BigEndian.PutUint16(c.cache[:], uint16(len(payload)))

	if _, err := c.Conn.Write(c.cache[:]); err != nil {
		return 0, err
	}

	return c.Conn.Write(payload)
}

func (c *vlessPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	total := len(b)
	if total == 0 {
		return 0, nil
	}

	if total <= maxLength {
		return c.writePacket(b)
	}

	offset := 0

	for offset < total {
		cursor := offset + maxLength
		if cursor > total {
			cursor = total
		}

		n, err := c.writePacket(b[offset:cursor])
		if err != nil {
			return offset + n, err
		}

		offset = cursor
		if offset == total {
			break
		}
	}

	return total, nil
}

func (c *vlessPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	if c.remain > 0 {
		length := len(b)
		if c.remain < length {
			length = c.remain
		}

		n, err := c.Conn.Read(b[:length])
		if err != nil {
			return 0, c.rAddr, err
		}

		c.remain -= n
		return n, c.rAddr, nil
	}

	if _, err := c.Conn.Read(b[:2]); err != nil {
		return 0, c.rAddr, err
	}

	total := int(binary.BigEndian.Uint16(b[:2]))
	if total == 0 {
		return 0, c.rAddr, nil
	}

	length := len(b)
	if length > total {
		length = total
	}

	if _, err := io.ReadFull(c.Conn, b[:length]); err != nil {
		return 0, c.rAddr, errors.New("read packet error")
	}

	c.remain = total - length

	return length, c.rAddr, nil
}

func NewVless(option VlessOption) (*Vless, error) {
	var addons *vless.Addons
	if option.Network != "ws" && len(option.Flow) >= 16 {
		option.Flow = option.Flow[:16]
		switch option.Flow {
		case vless.XRV:
			log.Warnln("To use %s, ensure your server is upgrade to Xray-core v1.8.0+", vless.XRV)
			fallthrough
		case vless.XRO, vless.XRD, vless.XRS:
			addons = &vless.Addons{
				Flow: option.Flow,
			}
		default:
			return nil, fmt.Errorf("unsupported xtls flow type: %s", option.Flow)
		}
	}

	switch option.PacketEncoding {
	case "packetaddr", "packet":
		option.PacketAddr = true
		option.XUDP = false
	default: // https://github.com/XTLS/Xray-core/pull/1567#issuecomment-1407305458
		if !option.PacketAddr {
			option.XUDP = true
		}
	}
	if option.XUDP {
		option.PacketAddr = false
	}

	client, err := vless.NewClient(option.UUID, addons, option.FlowShow)
	if err != nil {
		return nil, err
	}

	v := &Vless{
		Base: &Base{
			name:   option.Name,
			addr:   net.JoinHostPort(option.Server, strconv.Itoa(option.Port)),
			tp:     C.Vless,
			udp:    option.UDP,
			xudp:   option.XUDP,
			tfo:    option.TFO,
			iface:  option.Interface,
			rmark:  option.RoutingMark,
			prefer: C.NewDNSPrefer(option.IPVersion),
		},
		client: client,
		option: &option,
	}

	v.realityConfig, err = v.option.RealityOpts.Parse()
	if err != nil {
		return nil, err
	}

	switch option.Network {
	case "h2":
		if len(option.HTTP2Opts.Host) == 0 {
			option.HTTP2Opts.Host = append(option.HTTP2Opts.Host, "www.example.com")
		}
	case "grpc":
		dialFn := func(network, addr string) (net.Conn, error) {
			var err error
			var cDialer C.Dialer = dialer.NewDialer(v.Base.DialOptions()...)
			if len(v.option.DialerProxy) > 0 {
				cDialer, err = proxydialer.NewByName(v.option.DialerProxy, cDialer)
				if err != nil {
					return nil, err
				}
			}
			c, err := cDialer.DialContext(context.Background(), "tcp", v.addr)
			if err != nil {
				return nil, fmt.Errorf("%s connect error: %s", v.addr, err.Error())
			}
			tcpKeepAlive(c)
			return c, nil
		}

		gunConfig := &gun.Config{
			ServiceName:       v.option.GrpcOpts.GrpcServiceName,
			Host:              v.option.ServerName,
			ClientFingerprint: v.option.ClientFingerprint,
		}
		if option.ServerName == "" {
			gunConfig.Host = v.addr
		}
		var tlsConfig *tls.Config
		if option.TLS {
			tlsConfig = tlsC.GetGlobalTLSConfig(&tls.Config{
				InsecureSkipVerify: v.option.SkipCertVerify,
				ServerName:         v.option.ServerName,
			})
			if option.ServerName == "" {
				host, _, _ := net.SplitHostPort(v.addr)
				tlsConfig.ServerName = host
			}
		}

		v.gunTLSConfig = tlsConfig
		v.gunConfig = gunConfig

		v.transport = gun.NewHTTP2Client(dialFn, tlsConfig, v.option.ClientFingerprint, v.realityConfig)
	}

	return v, nil
}

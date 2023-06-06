package vmess

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dreamacro/clash/common/buf"
	N "github.com/Dreamacro/clash/common/net"
	tlsC "github.com/Dreamacro/clash/component/tls"

	"github.com/gorilla/websocket"
	"github.com/zhangyunhao116/fastrand"
)

type websocketConn struct {
	conn       *websocket.Conn
	reader     io.Reader
	remoteAddr net.Addr

	rawWriter N.ExtendedWriter

	// https://godoc.org/github.com/gorilla/websocket#hdr-Concurrency
	rMux sync.Mutex
	wMux sync.Mutex
}

type websocketWithEarlyDataConn struct {
	net.Conn
	wsWriter N.ExtendedWriter
	underlay net.Conn
	closed   bool
	dialed   chan bool
	cancel   context.CancelFunc
	ctx      context.Context
	config   *WebsocketConfig
}

type WebsocketConfig struct {
	Host                string
	Port                string
	Path                string
	Headers             http.Header
	TLS                 bool
	TLSConfig           *tls.Config
	MaxEarlyData        int
	EarlyDataHeaderName string
	ClientFingerprint   string
}

// Read implements net.Conn.Read()
func (wsc *websocketConn) Read(b []byte) (int, error) {
	wsc.rMux.Lock()
	defer wsc.rMux.Unlock()
	for {
		reader, err := wsc.getReader()
		if err != nil {
			return 0, err
		}

		nBytes, err := reader.Read(b)
		if err == io.EOF {
			wsc.reader = nil
			continue
		}
		return nBytes, err
	}
}

// Write implements io.Writer.
func (wsc *websocketConn) Write(b []byte) (int, error) {
	wsc.wMux.Lock()
	defer wsc.wMux.Unlock()
	if err := wsc.conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (wsc *websocketConn) WriteBuffer(buffer *buf.Buffer) error {
	var payloadBitLength int
	dataLen := buffer.Len()
	data := buffer.Bytes()
	if dataLen < 126 {
		payloadBitLength = 1
	} else if dataLen < 65536 {
		payloadBitLength = 3
	} else {
		payloadBitLength = 9
	}

	var headerLen int
	headerLen += 1 // FIN / RSV / OPCODE
	headerLen += payloadBitLength
	headerLen += 4 // MASK KEY

	header := buffer.ExtendHeader(headerLen)
	_ = header[2] // bounds check hint to compiler
	header[0] = websocket.BinaryMessage | 1<<7
	header[1] = 1 << 7

	if dataLen < 126 {
		header[1] |= byte(dataLen)
	} else if dataLen < 65536 {
		header[1] |= 126
		binary.BigEndian.PutUint16(header[2:], uint16(dataLen))
	} else {
		header[1] |= 127
		binary.BigEndian.PutUint64(header[2:], uint64(dataLen))
	}

	maskKey := fastrand.Uint32()
	binary.LittleEndian.PutUint32(header[1+payloadBitLength:], maskKey)
	N.MaskWebSocket(maskKey, data)

	wsc.wMux.Lock()
	defer wsc.wMux.Unlock()
	return wsc.rawWriter.WriteBuffer(buffer)
}

func (wsc *websocketConn) FrontHeadroom() int {
	return 14
}

func (wsc *websocketConn) Upstream() any {
	return wsc.conn.UnderlyingConn()
}

func (wsc *websocketConn) Close() error {
	var e []string
	if err := wsc.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second*5)); err != nil {
		e = append(e, err.Error())
	}
	if err := wsc.conn.Close(); err != nil {
		e = append(e, err.Error())
	}
	if len(e) > 0 {
		return fmt.Errorf("failed to close connection: %s", strings.Join(e, ","))
	}
	return nil
}

func (wsc *websocketConn) getReader() (io.Reader, error) {
	if wsc.reader != nil {
		return wsc.reader, nil
	}

	_, reader, err := wsc.conn.NextReader()
	if err != nil {
		return nil, err
	}
	wsc.reader = reader
	return reader, nil
}

func (wsc *websocketConn) LocalAddr() net.Addr {
	return wsc.conn.LocalAddr()
}

func (wsc *websocketConn) RemoteAddr() net.Addr {
	return wsc.remoteAddr
}

func (wsc *websocketConn) SetDeadline(t time.Time) error {
	if err := wsc.SetReadDeadline(t); err != nil {
		return err
	}
	return wsc.SetWriteDeadline(t)
}

func (wsc *websocketConn) SetReadDeadline(t time.Time) error {
	return wsc.conn.SetReadDeadline(t)
}

func (wsc *websocketConn) SetWriteDeadline(t time.Time) error {
	return wsc.conn.SetWriteDeadline(t)
}

func (wsedc *websocketWithEarlyDataConn) Dial(earlyData []byte) error {
	base64DataBuf := &bytes.Buffer{}
	base64EarlyDataEncoder := base64.NewEncoder(base64.RawURLEncoding, base64DataBuf)

	earlyDataBuf := bytes.NewBuffer(earlyData)
	if _, err := base64EarlyDataEncoder.Write(earlyDataBuf.Next(wsedc.config.MaxEarlyData)); err != nil {
		return fmt.Errorf("failed to encode early data: %w", err)
	}

	if errc := base64EarlyDataEncoder.Close(); errc != nil {
		return fmt.Errorf("failed to encode early data tail: %w", errc)
	}

	var err error
	if wsedc.Conn, err = streamWebsocketConn(wsedc.ctx, wsedc.underlay, wsedc.config, base64DataBuf); err != nil {
		wsedc.Close()
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	wsedc.dialed <- true
	wsedc.wsWriter = N.NewExtendedWriter(wsedc.Conn)
	if earlyDataBuf.Len() != 0 {
		_, err = wsedc.Conn.Write(earlyDataBuf.Bytes())
	}

	return err
}

func (wsedc *websocketWithEarlyDataConn) Write(b []byte) (int, error) {
	if wsedc.closed {
		return 0, io.ErrClosedPipe
	}
	if wsedc.Conn == nil {
		if err := wsedc.Dial(b); err != nil {
			return 0, err
		}
		return len(b), nil
	}

	return wsedc.Conn.Write(b)
}

func (wsedc *websocketWithEarlyDataConn) WriteBuffer(buffer *buf.Buffer) error {
	if wsedc.closed {
		return io.ErrClosedPipe
	}
	if wsedc.Conn == nil {
		if err := wsedc.Dial(buffer.Bytes()); err != nil {
			return err
		}
		return nil
	}

	return wsedc.wsWriter.WriteBuffer(buffer)
}

func (wsedc *websocketWithEarlyDataConn) Read(b []byte) (int, error) {
	if wsedc.closed {
		return 0, io.ErrClosedPipe
	}
	if wsedc.Conn == nil {
		select {
		case <-wsedc.ctx.Done():
			return 0, io.ErrUnexpectedEOF
		case <-wsedc.dialed:
		}
	}
	return wsedc.Conn.Read(b)
}

func (wsedc *websocketWithEarlyDataConn) Close() error {
	wsedc.closed = true
	wsedc.cancel()
	if wsedc.Conn == nil {
		return nil
	}
	return wsedc.Conn.Close()
}

func (wsedc *websocketWithEarlyDataConn) LocalAddr() net.Addr {
	if wsedc.Conn == nil {
		return wsedc.underlay.LocalAddr()
	}
	return wsedc.Conn.LocalAddr()
}

func (wsedc *websocketWithEarlyDataConn) RemoteAddr() net.Addr {
	if wsedc.Conn == nil {
		return wsedc.underlay.RemoteAddr()
	}
	return wsedc.Conn.RemoteAddr()
}

func (wsedc *websocketWithEarlyDataConn) SetDeadline(t time.Time) error {
	if err := wsedc.SetReadDeadline(t); err != nil {
		return err
	}
	return wsedc.SetWriteDeadline(t)
}

func (wsedc *websocketWithEarlyDataConn) SetReadDeadline(t time.Time) error {
	if wsedc.Conn == nil {
		return nil
	}
	return wsedc.Conn.SetReadDeadline(t)
}

func (wsedc *websocketWithEarlyDataConn) SetWriteDeadline(t time.Time) error {
	if wsedc.Conn == nil {
		return nil
	}
	return wsedc.Conn.SetWriteDeadline(t)
}

func (wsedc *websocketWithEarlyDataConn) FrontHeadroom() int {
	return 14
}

func (wsedc *websocketWithEarlyDataConn) Upstream() any {
	return wsedc.underlay
}

//func (wsedc *websocketWithEarlyDataConn) LazyHeadroom() bool {
//	return wsedc.Conn == nil
//}
//
//func (wsedc *websocketWithEarlyDataConn) Upstream() any {
//	if wsedc.Conn == nil { // ensure return a nil interface not an interface with nil value
//		return nil
//	}
//	return wsedc.Conn
//}

func (wsedc *websocketWithEarlyDataConn) NeedHandshake() bool {
	return wsedc.Conn == nil
}

func streamWebsocketWithEarlyDataConn(conn net.Conn, c *WebsocketConfig) (net.Conn, error) {
	ctx, cancel := context.WithCancel(context.Background())
	conn = &websocketWithEarlyDataConn{
		dialed:   make(chan bool, 1),
		cancel:   cancel,
		ctx:      ctx,
		underlay: conn,
		config:   c,
	}
	// websocketWithEarlyDataConn can't correct handle Deadline
	// it will not apply the already set Deadline after Dial()
	// so call N.NewDeadlineConn to add a safe wrapper
	return N.NewDeadlineConn(conn), nil
}

func streamWebsocketConn(ctx context.Context, conn net.Conn, c *WebsocketConfig, earlyData *bytes.Buffer) (net.Conn, error) {

	dialer := &websocket.Dialer{
		NetDial: func(network, addr string) (net.Conn, error) {
			return conn, nil
		},
		ReadBufferSize:   4 * 1024,
		WriteBufferSize:  4 * 1024,
		HandshakeTimeout: time.Second * 8,
	}

	scheme := "ws"
	if c.TLS {
		scheme = "wss"
		dialer.TLSClientConfig = c.TLSConfig
		if len(c.ClientFingerprint) != 0 {
			if fingerprint, exists := tlsC.GetFingerprint(c.ClientFingerprint); exists {
				dialer.NetDialTLSContext = func(_ context.Context, _, addr string) (net.Conn, error) {
					utlsConn := tlsC.UClient(conn, c.TLSConfig, fingerprint)

					if err := utlsConn.(*tlsC.UConn).WebsocketHandshake(); err != nil {
						return nil, fmt.Errorf("parse url %s error: %w", c.Path, err)
					}
					return utlsConn, nil
				}
			}
		}
	}

	u, err := url.Parse(c.Path)
	if err != nil {
		return nil, fmt.Errorf("parse url %s error: %w", c.Path, err)
	}

	uri := url.URL{
		Scheme:   scheme,
		Host:     net.JoinHostPort(c.Host, c.Port),
		Path:     u.Path,
		RawQuery: u.RawQuery,
	}

	headers := http.Header{}
	if c.Headers != nil {
		for k := range c.Headers {
			headers.Add(k, c.Headers.Get(k))
		}
	}

	if earlyData != nil {
		if c.EarlyDataHeaderName == "" {
			uri.Path += earlyData.String()
		} else {
			headers.Set(c.EarlyDataHeaderName, earlyData.String())
		}
	}

	wsConn, resp, err := dialer.DialContext(ctx, uri.String(), headers)
	if err != nil {
		reason := err
		if resp != nil {
			reason = errors.New(resp.Status)
		}
		return nil, fmt.Errorf("dial %s error: %w", uri.Host, reason)
	}

	conn = &websocketConn{
		conn:       wsConn,
		rawWriter:  N.NewExtendedWriter(wsConn.UnderlyingConn()),
		remoteAddr: conn.RemoteAddr(),
	}
	// websocketConn can't correct handle ReadDeadline
	// gorilla/websocket will cache the os.ErrDeadlineExceeded from conn.Read()
	// it will cause read fail and event panic in *websocket.Conn.NextReader()
	// so call N.NewDeadlineConn to add a safe wrapper
	return N.NewDeadlineConn(conn), nil
}

func StreamWebsocketConn(ctx context.Context, conn net.Conn, c *WebsocketConfig) (net.Conn, error) {
	if u, err := url.Parse(c.Path); err == nil {
		if q := u.Query(); q.Get("ed") != "" {
			if ed, err := strconv.Atoi(q.Get("ed")); err == nil {
				c.MaxEarlyData = ed
				c.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
				q.Del("ed")
				u.RawQuery = q.Encode()
				c.Path = u.String()
			}
		}
	}

	if c.MaxEarlyData > 0 {
		return streamWebsocketWithEarlyDataConn(conn, c)
	}

	return streamWebsocketConn(ctx, conn, c, nil)
}

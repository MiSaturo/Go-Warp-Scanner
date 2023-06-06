package inbound

import (
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/listener/http"
	"github.com/Dreamacro/clash/log"
)

type HTTPOption struct {
	BaseOption
}

func (o HTTPOption) Equal(config C.InboundConfig) bool {
	return optionToString(o) == optionToString(config)
}

type HTTP struct {
	*Base
	config *HTTPOption
	l      *http.Listener
}

func NewHTTP(options *HTTPOption) (*HTTP, error) {
	base, err := NewBase(&options.BaseOption)
	if err != nil {
		return nil, err
	}
	return &HTTP{
		Base:   base,
		config: options,
	}, nil
}

// Config implements constant.InboundListener
func (h *HTTP) Config() C.InboundConfig {
	return h.config
}

// Address implements constant.InboundListener
func (h *HTTP) Address() string {
	return h.l.Address()
}

// Listen implements constant.InboundListener
func (h *HTTP) Listen(tcpIn chan<- C.ConnContext, udpIn chan<- C.PacketAdapter, natTable C.NatTable) error {
	var err error
	h.l, err = http.New(h.RawAddress(), tcpIn, h.Additions()...)
	if err != nil {
		return err
	}
	log.Infoln("HTTP[%s] proxy listening at: %s", h.Name(), h.Address())
	return nil
}

// Close implements constant.InboundListener
func (h *HTTP) Close() error {
	if h.l != nil {
		return h.l.Close()
	}
	return nil
}

var _ C.InboundListener = (*HTTP)(nil)

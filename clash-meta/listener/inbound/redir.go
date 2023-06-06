package inbound

import (
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/listener/redir"
	"github.com/Dreamacro/clash/log"
)

type RedirOption struct {
	BaseOption
}

func (o RedirOption) Equal(config C.InboundConfig) bool {
	return optionToString(o) == optionToString(config)
}

type Redir struct {
	*Base
	config *RedirOption
	l      *redir.Listener
}

func NewRedir(options *RedirOption) (*Redir, error) {
	base, err := NewBase(&options.BaseOption)
	if err != nil {
		return nil, err
	}
	return &Redir{
		Base:   base,
		config: options,
	}, nil
}

// Config implements constant.InboundListener
func (r *Redir) Config() C.InboundConfig {
	return r.config
}

// Address implements constant.InboundListener
func (r *Redir) Address() string {
	return r.l.Address()
}

// Listen implements constant.InboundListener
func (r *Redir) Listen(tcpIn chan<- C.ConnContext, udpIn chan<- C.PacketAdapter, natTable C.NatTable) error {
	var err error
	r.l, err = redir.New(r.RawAddress(), tcpIn, r.Additions()...)
	if err != nil {
		return err
	}
	log.Infoln("Redir[%s] proxy listening at: %s", r.Name(), r.Address())
	return nil
}

// Close implements constant.InboundListener
func (r *Redir) Close() error {
	if r.l != nil {
		r.l.Close()
	}
	return nil
}

var _ C.InboundListener = (*Redir)(nil)

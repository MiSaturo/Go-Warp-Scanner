package config

import (
	"encoding/json"
)

type ShadowsocksServer struct {
	Enable   bool
	Listen   string
	Password string
	Cipher   string
	Udp      bool
}

func (t ShadowsocksServer) String() string {
	b, _ := json.Marshal(t)
	return string(b)
}

package json

import (
	"github.com/bytedance/sonic"
)

const sonicName = "json"

// Codec that encodes to and decodes from JSON.
var Sonic = new(sonicCodec)

type sonicCodec int

func (j sonicCodec) Marshal(v interface{}) ([]byte, error) {
	return sonic.Marshal(v)
}

func (j sonicCodec) Unmarshal(b []byte, v interface{}) error {
	return sonic.Unmarshal(b, v)
}

func (j sonicCodec) Name() string {
	return sonicName
}

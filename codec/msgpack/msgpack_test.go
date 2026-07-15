package msgpack

import (
	"testing"

	"github.com/clakeboy/storm-rev/v2/codec/internal"
)

func TestMsgpack(t *testing.T) {
	internal.RoundtripTester(t, Codec)
}

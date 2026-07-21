package codec_test

import (
	"fmt"

	storm "github.com/clakeboy/storm-rev/v2"
	"github.com/clakeboy/storm-rev/v2/codec/gob"
	"github.com/clakeboy/storm-rev/v2/codec/json"
	"github.com/clakeboy/storm-rev/v2/codec/msgpack"
	"github.com/clakeboy/storm-rev/v2/codec/protobuf"
	"github.com/clakeboy/storm-rev/v2/codec/sereal"
)

func Example() {
	// The examples below show how to set up all the codecs shipped with Storm.
	// Proper error handling left out to make it simple.
	var gobDb, _ = storm.Open("gob.db", storm.Codec(gob.Codec))
	var jsonDb, _ = storm.Open("json.db", storm.Codec(json.Codec))
	var msgpackDb, _ = storm.Open("msgpack.db", storm.Codec(msgpack.Codec))
	var serealDb, _ = storm.Open("sereal.db", storm.Codec(sereal.Codec))
	var protobufDb, _ = storm.Open("protobuf.db", storm.Codec(protobuf.Codec))
	defer gobDb.Close()
	defer jsonDb.Close()
	defer msgpackDb.Close()
	defer serealDb.Close()
	defer protobufDb.Close()

	fmt.Printf("%T\n", gobDb.Codec())
	fmt.Printf("%T\n", jsonDb.Codec())
	fmt.Printf("%T\n", msgpackDb.Codec())
	fmt.Printf("%T\n", serealDb.Codec())
	fmt.Printf("%T\n", protobufDb.Codec())

	// Output:
	// *gob.gobCodec
	// *json.jsonCodec
	// *msgpack.msgpackCodec
	// *sereal.serealCodec
	// *protobuf.protobufCodec
}

package types

import (
	"fmt"

	"github.com/streamingfast/bstream"
	pbbstream "github.com/streamingfast/pbgo/sf/bstream/v1"
	pbsui "github.com/ticketland-io/firehose-sui/types/pb/sui"
	"google.golang.org/protobuf/proto"
)

func BlockFromProto(b *pbsui.Block) (*bstream.Block, error) {
	content, err := proto.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal to binary form: %s", err)
	}

	block := &bstream.Block{
		Id:             b.ID(),
		Number:         b.Number(),
		PreviousId:     b.PreviousID(),
		Timestamp:      b.Time(),
		LibNum:         b.LIBNum(),
		PayloadKind:    pbbstream.Protocol_UNKNOWN,
		PayloadVersion: 1,
	}

	return bstream.GetBlockPayloadSetter(block, content)
}

package api

import (
	"github.com/oasislabs/oasis-core/go/common/cbor"
	"github.com/oasislabs/oasis-core/go/common/crypto/signature"
	epochtime "github.com/oasislabs/oasis-core/go/epochtime/api"
)

// FreezeForever is an epoch that can be used to freeze a node for
// all (practical) time.
const FreezeForever epochtime.EpochTime = 0xffffffffffffffff

// NodeStatus is live status of a node.
type NodeStatus struct {
	// FreezeEndTime is the epoch when a frozen node can become unfrozen.
	//
	// After the specified epoch passes, this flag needs to be explicitly
	// cleared (set to zero) in order for the node to become unfrozen.
	FreezeEndTime epochtime.EpochTime `json:"freeze_end_time"`
}

// IsFrozen returns true if the node is currently frozen (prevented
// from being considered in scheduling decisions).
func (ns NodeStatus) IsFrozen() bool {
	return ns.FreezeEndTime > 0
}

// Unfreeze makes the node unfrozen.
func (ns *NodeStatus) Unfreeze() {
	ns.FreezeEndTime = 0
}

// UnfreezeNode is a request to unfreeze a frozen node.
type UnfreezeNode struct {
	NodeID    signature.PublicKey `json:"node_id"`
	Timestamp uint64              `json:"timestamp"`
}

// MarshalCBOR serializes the UnfreezeNode type into a CBOR byte vector.
func (u *UnfreezeNode) MarshalCBOR() []byte {
	return cbor.Marshal(u)
}

// UnmarshalCBOR deserializes a CBOR byte vector into a UnfreezeNode.
func (u *UnfreezeNode) UnmarshalCBOR(data []byte) error {
	return cbor.Unmarshal(data, u)
}

// SignedUnfreezeNode is a signed UnfreezeNode.
type SignedUnfreezeNode struct {
	signature.Signed
}

// Open first verifies the blob signature and then unmarshals the blob.
func (s *SignedUnfreezeNode) Open(context []byte, unfreeze *UnfreezeNode) error { // nolint: interfacer
	return s.Signed.Open(context, unfreeze)
}

// SignUnfreezeNode serializes the UnfreezeNode and signs the result.
func SignUnfreezeNode(signer signature.Signer, context []byte, unfreeze *UnfreezeNode) (*SignedUnfreezeNode, error) {
	signed, err := signature.SignSigned(signer, context, unfreeze)
	if err != nil {
		return nil, err
	}

	return &SignedUnfreezeNode{
		Signed: *signed,
	}, nil
}

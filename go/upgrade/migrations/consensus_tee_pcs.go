package migrations

import (
	"fmt"

	"github.com/oasisprotocol/oasis-core/go/common/node"
	abciAPI "github.com/oasisprotocol/oasis-core/go/consensus/tendermint/api"
	registryState "github.com/oasisprotocol/oasis-core/go/consensus/tendermint/apps/registry/state"
)

const (
	// ConsensusTEEPCSHandler is the name of the upgrade that enables PCS-based attestation support
	// for Intel SGX-based TEEs.
	ConsensusTEEPCSHandler = "consensus-tee-pcs"
)

var _ Handler = (*teePcsHandler)(nil)

type teePcsHandler struct{}

func (th *teePcsHandler) StartupUpgrade(ctx *Context) error {
	return nil
}

func (th *teePcsHandler) ConsensusUpgrade(ctx *Context, privateCtx interface{}) error {
	abciCtx := privateCtx.(*abciAPI.Context)
	switch abciCtx.Mode() {
	case abciAPI.ContextBeginBlock:
		// Nothing to do during begin block.
	case abciAPI.ContextEndBlock:
		// Update a consensus parameter during EndBlock.
		state := registryState.NewMutableState(abciCtx.State())

		params, err := state.ConsensusParameters(abciCtx)
		if err != nil {
			return fmt.Errorf("unable to load registry consensus parameters: %w", err)
		}

		params.TEEFeatures = &node.TEEFeatures{
			SGX: node.TEEFeaturesSGX{
				PCS: true,
			},
		}

		if err = state.SetConsensusParameters(abciCtx, params); err != nil {
			return fmt.Errorf("failed to update registry consensus parameters: %w", err)
		}
	default:
		return fmt.Errorf("upgrade handler called in unexpected context: %s", abciCtx.Mode())
	}
	return nil
}

func init() {
	Register(ConsensusTEEPCSHandler, &teePcsHandler{})
}

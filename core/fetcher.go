package core

import (
	"context"
	"fmt"
	"github.com/gogo/protobuf/proto"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	coregrpc "github.com/tendermint/tendermint/rpc/grpc"
	"io"

	logging "github.com/ipfs/go-log/v2"
	coretypes "github.com/tendermint/tendermint/rpc/core/types"
	"github.com/tendermint/tendermint/types"

	libhead "github.com/celestiaorg/go-header"
)

const newBlockSubscriber = "NewBlock/Events"

var (
	log                     = logging.Logger("core")
	newDataSignedBlockQuery = types.QueryForEvent(types.EventSignedBlock).String()
)

type BlockFetcher struct {
	client Client

	doneCh chan struct{}
	cancel context.CancelFunc
}

// NewBlockFetcher returns a new `BlockFetcher`.
func NewBlockFetcher(client Client) *BlockFetcher {
	return &BlockFetcher{
		client: client,
	}
}

func (f *BlockFetcher) Stop(ctx context.Context) error {
	f.cancel()
	select {
	case <-f.doneCh:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("fetcher: unsubscribe from new block events: %w", ctx.Err())
	}
}

// GetBlockInfo queries Core for additional block information, like Commit and ValidatorSet.
func (f *BlockFetcher) GetBlockInfo(ctx context.Context, height *int64) (*types.Commit, *types.ValidatorSet, error) {
	commit, err := f.Commit(ctx, height)
	if err != nil {
		return nil, nil, fmt.Errorf("core/fetcher: getting commit at height %d: %w", height, err)
	}

	// If a nil `height` is given as a parameter, there is a chance
	// that a new block could be produced between getting the latest
	// commit and getting the latest validator set. Therefore, it is
	// best to get the validator set at the latest commit's height to
	// prevent this potential inconsistency.
	valSet, err := f.ValidatorSet(ctx, &commit.Height)
	if err != nil {
		return nil, nil, fmt.Errorf("core/fetcher: getting validator set at height %d: %w", height, err)
	}

	return commit, valSet, nil
}

// GetBlock queries Core for a `Block` at the given height.
// if the height is nil, use the latest height
func (f *BlockFetcher) GetBlock(ctx context.Context, height *int64) (*types.Block, error) {
	blockHeight, err := f.resolveHeight(ctx, height)
	if err != nil {
		return nil, err
	}

	stream, err := f.client.BlockByHeight(ctx, &coregrpc.BlockByHeightRequest{Height: blockHeight})
	if err != nil {
		return nil, err
	}
	block, _, _, _, err := receiveBlockByHeight(stream)
	if err != nil {
		return nil, err
	}
	return block, nil
}

func (f *BlockFetcher) GetBlockByHash(ctx context.Context, hash libhead.Hash) (*types.Block, error) {
	if hash == nil {
		return nil, fmt.Errorf("cannot get block with nil hash")
	}
	stream, err := f.client.BlockByHash(ctx, &coregrpc.BlockByHashRequest{Hash: hash})
	if err != nil {
		return nil, err
	}
	block, err := receiveBlockByHash(stream)
	if err != nil {
		return nil, err
	}

	return block, nil
}

// resolveHeight takes a height pointer and returns its value if it's not nil.
// otherwise, returns the latest height.
func (f *BlockFetcher) resolveHeight(ctx context.Context, height *int64) (int64, error) {
	if height != nil {
		return *height, nil
	} else {
		status, err := f.client.Status(ctx, &coregrpc.StatusRequest{})
		if err != nil {
			return 0, err
		}
		return status.SyncInfo.LatestBlockHeight, nil
	}
}

// GetSignedBlock queries Core for a `Block` at the given height.
// if the height is nil, use the latest height.
func (f *BlockFetcher) GetSignedBlock(ctx context.Context, height *int64) (*coretypes.ResultSignedBlock, error) {
	blockHeight, err := f.resolveHeight(ctx, height)
	if err != nil {
		return nil, err
	}

	stream, err := f.client.BlockByHeight(ctx, &coregrpc.BlockByHeightRequest{Height: blockHeight})
	if err != nil {
		return nil, err
	}
	block, _, commit, validatorSet, err := receiveBlockByHeight(stream)
	if err != nil {
		return nil, err
	}
	return &coretypes.ResultSignedBlock{
		Header:       block.Header,
		Commit:       *commit,
		Data:         block.Data,
		ValidatorSet: *validatorSet,
	}, nil
}

// Commit queries Core for a `Commit` from the block at
// the given height.
// If the height is nil, use the latest height.
func (f *BlockFetcher) Commit(ctx context.Context, height *int64) (*types.Commit, error) {
	blockHeight, err := f.resolveHeight(ctx, height)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Commit(ctx, &coregrpc.CommitRequest{Height: blockHeight})
	if err != nil {
		return nil, err
	}

	if res != nil && res.Commit == nil {
		return nil, fmt.Errorf("core/fetcher: commit not found at height %d", height)
	}

	commit, err := types.CommitFromProto(res.Commit)
	if err != nil {
		return nil, err
	}

	return commit, nil
}

// ValidatorSet queries Core for the ValidatorSet from the
// block at the given height.
// If the height is nil, use the latest height.
func (f *BlockFetcher) ValidatorSet(ctx context.Context, height *int64) (*types.ValidatorSet, error) {
	blockHeight, err := f.resolveHeight(ctx, height)
	if err != nil {
		return nil, err
	}
	res, err := f.client.ValidatorSet(ctx, &coregrpc.ValidatorSetRequest{Height: blockHeight})
	if err != nil {
		return nil, err
	}

	if res != nil && res.ValidatorSet == nil {
		return nil, fmt.Errorf("core/fetcher: validator set not found at height %d", height)
	}

	validatorSet, err := types.ValidatorSetFromProto(res.ValidatorSet)
	if err != nil {
		return nil, err
	}

	return validatorSet, nil
}

// SubscribeNewBlockEvent subscribes to new block events from Core, returning
// a new block event channel on success.
func (f *BlockFetcher) SubscribeNewBlockEvent(ctx context.Context) (<-chan types.EventDataSignedBlock, error) {
	ctx, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	f.doneCh = make(chan struct{})

	subscription, err := f.client.SubscribeNewHeights(ctx, &coregrpc.SubscribeNewHeightsRequest{})
	if err != nil {
		return nil, err
	}

	signedBlockCh := make(chan types.EventDataSignedBlock)
	go func() {
		defer close(f.doneCh)
		defer close(signedBlockCh)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				resp, err := subscription.Recv()
				if err != nil {
					if ctx.Err() == nil {
						log.Errorw("fetcher: error receiving new height", "err", err.Error())
					}
					return
				}
				signedBlock, err := f.GetSignedBlock(ctx, &resp.Height)
				if err != nil {
					log.Errorw("fetcher: error receiving signed block", "height", resp.Height, "err", err.Error())
					return
				}
				select {
				case signedBlockCh <- types.EventDataSignedBlock{
					Header:       signedBlock.Header,
					Commit:       signedBlock.Commit,
					ValidatorSet: signedBlock.ValidatorSet,
					Data:         signedBlock.Data,
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return signedBlockCh, nil
}

// IsSyncing returns the sync status of the Core connection: true for
// syncing, and false for already caught up. It can also return an error
// in the case of a failed status request.
func (f *BlockFetcher) IsSyncing(ctx context.Context) (bool, error) {
	resp, err := f.client.Status(ctx, &coregrpc.StatusRequest{})
	if err != nil {
		return false, err
	}
	return resp.SyncInfo.CatchingUp, nil
}

func receiveBlockByHeight(streamer coregrpc.BlockAPI_BlockByHeightClient) (*types.Block, *types.BlockMeta, *types.Commit, *types.ValidatorSet, error) {
	parts := make([]*tmproto.Part, 0)

	// receive the first part to get the block meta, commit, and validator set
	resp, err := streamer.Recv()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	blockMeta, err := types.BlockMetaFromProto(resp.BlockMeta)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	commit, err := types.CommitFromProto(resp.Commit)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	validatorSet, err := types.ValidatorSetFromProto(resp.ValidatorSet)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	parts = append(parts, resp.BlockPart)

	// receive the rest of the block
	isLast := resp.IsLast
	for !isLast {
		resp, err := streamer.Recv()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		parts = append(parts, resp.BlockPart)
		isLast = resp.IsLast
	}
	block, err := partsToBlock(parts)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return block, blockMeta, commit, validatorSet, nil
}

func receiveBlockByHash(streamer coregrpc.BlockAPI_BlockByHashClient) (*types.Block, error) {
	parts := make([]*tmproto.Part, 0)
	isLast := false
	for !isLast {
		resp, err := streamer.Recv()
		if err != nil {
			return nil, err
		}
		parts = append(parts, resp.BlockPart)
		isLast = resp.IsLast
	}
	return partsToBlock(parts)
}

func partsToBlock(parts []*tmproto.Part) (*types.Block, error) {
	partSet := types.NewPartSetFromHeader(types.PartSetHeader{
		Total: uint32(len(parts)),
	})
	for _, part := range parts {
		ok, err := partSet.AddPartWithoutProof(&types.Part{Index: part.Index, Bytes: part.Bytes})
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, err
		}
	}
	pbb := new(tmproto.Block)
	bz, err := io.ReadAll(partSet.GetReader())
	if err != nil {
		return nil, err
	}
	err = proto.Unmarshal(bz, pbb)
	if err != nil {
		return nil, err
	}
	block, err := types.BlockFromProto(pbb)
	if err != nil {
		return nil, err
	}
	return block, nil
}

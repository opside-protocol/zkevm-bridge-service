package claimtxman

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	ctmtypes "github.com/0xPolygonHermez/zkevm-bridge-service/claimtxman/types"
	"github.com/0xPolygonHermez/zkevm-bridge-service/etherman"
	"github.com/0xPolygonHermez/zkevm-bridge-service/utils"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/jackc/pgx/v4"
)

const (
	maxHistorySize  = 10
	keyLen          = 32
	mtHeight        = 32
	cacheSize       = 1000
	LeafTypeMessage = uint8(1)
	maxRetries      = 10
)

// ClaimTxManager is the claim transaction manager for L2.
type ClaimTxManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	// client is the ethereum client
	l1Node          *utils.Client
	l2Node          *utils.Client
	networkID       uint
	bridgeService   bridgeServiceInterface
	cfg             Config
	chExitRootEvent chan *etherman.GlobalExitRoot
	storage         storageInterface
	l1Auth          *bind.TransactOpts
	l2Auth          *bind.TransactOpts
	nonceCache      *lru.Cache[string, uint64]
	l1NodeURL       string
}

// NewClaimTxManager creates a new claim transaction manager.
func NewClaimTxManager(cfg Config, chExitRootEvent chan *etherman.GlobalExitRoot, l1NodeURL string, l2NodeURL string, networkID uint, l1BridgeAddr common.Address, l2BridgeAddr common.Address, bridgeService bridgeServiceInterface, storage interface{}) (*ClaimTxManager, error) {
	l1Client, err := utils.NewClient(context.Background(), l1NodeURL, l1BridgeAddr)
	l2Client, err := utils.NewClient(context.Background(), l2NodeURL, l2BridgeAddr)
	if err != nil {
		return nil, err
	}
	cache, err := lru.New[string, uint64](int(cacheSize))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l1Auth, err := l1Client.GetSignerFromKeystore(ctx, cfg.PrivateKeyL1)
	l2Auth, err := l1Client.GetSignerFromKeystore(ctx, cfg.PrivateKeyL2)
	return &ClaimTxManager{
		ctx:             ctx,
		cancel:          cancel,
		l1Node:          l1Client,
		l2Node:          l2Client,
		networkID:       networkID,
		bridgeService:   bridgeService,
		cfg:             cfg,
		chExitRootEvent: chExitRootEvent,
		storage:         storage.(storageInterface),
		l1Auth:          l1Auth,
		l2Auth:          l2Auth,
		nonceCache:      cache,
		l1NodeURL:       l1NodeURL,
	}, err
}

func (tm *ClaimTxManager) GetAuth(isL1 bool) *bind.TransactOpts {
	if isL1 {
		return tm.l1Auth
	}
	return tm.l2Auth
}

func (tm *ClaimTxManager) GetNode(isL1 bool) *utils.Client {
	if isL1 {
		return tm.l1Node
	}
	return tm.l2Node
}

func (tm *ClaimTxManager) GetDestinationL1Node(l1BridgeAddr common.Address) (*utils.Client, error) {
	l1Client, err := utils.NewClient(context.Background(), tm.l1NodeURL, l1BridgeAddr)
	if err != nil {
		return nil, err
	}
	return l1Client, nil
}

// Start will start the tx management, reading txs from storage,
// send then to the blockchain and keep monitoring them until they
// get mined
func (tm *ClaimTxManager) Start() {
	go func() {
		for {
			select {
			case <-tm.ctx.Done():
				return
			default:
			}

			num, err := tm.monitorTxs(context.Background())
			if err != nil {
				log.Errorf("failed to monitor txs: %v", err)
			}

			if num == 0 {
				time.Sleep(tm.cfg.FrequencyToMonitorTxs.Duration)
			}
		}
	}()
	for {
		select {
		case <-tm.ctx.Done():
			return
		case ger := <-tm.chExitRootEvent:
			go func() {
				err := tm.updateDepositsStatus(ger)
				if err != nil {
					log.Errorf("failed to update deposits status: %v", err)
				}
			}()
			// case <-time.After(tm.cfg.FrequencyToMonitorTxs.Duration):
			// 	err := tm.monitorTxs(context.Background())
			// 	if err != nil {
			// 		log.Errorf("failed to monitor txs: %v", err)
			// 	}
		}
	}
}

func (tm *ClaimTxManager) updateDepositsStatus(ger *etherman.GlobalExitRoot) error {
	dbTx, err := tm.storage.BeginDBTransaction(tm.ctx)
	if err != nil {
		return err
	}
	err = tm.processDepositStatus(ger, dbTx)
	if err != nil {
		log.Errorf("error processing ger. Error: %v", err)
		rollbackErr := tm.storage.Rollback(tm.ctx, dbTx)
		if rollbackErr != nil {
			log.Errorf("claimtxman error rolling back state. RollbackErr: %v, err: %s", rollbackErr, err.Error())
			return rollbackErr
		}
		return err
	}
	err = tm.storage.Commit(tm.ctx, dbTx)
	if err != nil {
		log.Errorf("AddClaimTx committing dbTx. Err: %v", err)
		rollbackErr := tm.storage.Rollback(tm.ctx, dbTx)
		if rollbackErr != nil {
			log.Fatalf("claimtxman error rolling back state. RollbackErr: %s, err: %s", rollbackErr.Error(), err.Error())
		}
		log.Fatalf("AddClaimTx committing dbTx, err: %s", err.Error())
	}
	return nil
}

func (tm *ClaimTxManager) processDepositStatus(ger *etherman.GlobalExitRoot, dbTx pgx.Tx) error {
	if ger.BlockID != 0 { // L2 exit root is updated
		log.Infof("Rollup exitroot %v is updated", ger.ExitRoots[1])
		deposits, err := tm.storage.UpdateDepositsStatus(tm.ctx, ger.ExitRoots[1][:], tm.networkID, dbTx)
		if err != nil {
			log.Errorf("error updating L2DepositsStatus. Error: %v", err)
			return err
		}
		if tm.cfg.EnabledCross {
			return tm.handleDeposits(deposits, true, dbTx)
		}
	} else { // L1 exit root is updated in the trusted state
		log.Infof("Mainnet exitroot %v is updated", ger.ExitRoots[0])
		deposits, err := tm.storage.UpdateDepositsStatus(tm.ctx, ger.ExitRoots[0][:], 0, dbTx)
		if err != nil {
			log.Errorf("error getting and updating L1DepositsStatus. Error: %v", err)
			return err
		}
		return tm.handleDeposits(deposits, false, dbTx)
	}
	return nil
}

func (tm *ClaimTxManager) handleDeposits(deposits []*etherman.Deposit, isL1 bool, dbTx pgx.Tx) error {
	for _, deposit := range deposits {
		if deposit.NetworkID == 1 && deposit.DestinationNetwork == 0 {
			continue
		}
		claimHash, err := tm.bridgeService.GetDepositStatus(tm.ctx, deposit.DepositCount, deposit.DestinationNetwork)
		if err != nil {
			log.Errorf("error getting deposit status for deposit %d. Error: %v", deposit.DepositCount, err)
			return err
		}
		if len(claimHash) > 0 || deposit.LeafType == LeafTypeMessage {
			log.Infof("Ignoring deposit: %d, leafType: %d, claimHash: %s", deposit.DepositCount, deposit.LeafType, claimHash)
			continue
		}
		log.Infof("create the claim tx for the deposit %d", deposit.DepositCount)
		ger, proves, err := tm.bridgeService.GetClaimProof(deposit.DepositCount, deposit.NetworkID, dbTx)
		if err != nil {
			log.Errorf("error getting Claim Proof for deposit %d. Error: %v", deposit.DepositCount, err)
			return err
		}
		var mtProves [mtHeight][keyLen]byte
		for i := 0; i < mtHeight; i++ {
			mtProves[i] = proves[i]
		}
		tx, err := tm.GetNode(isL1).BuildSendClaim(tm.ctx, deposit, mtProves,
			&etherman.GlobalExitRoot{
				ExitRoots: []common.Hash{
					ger.ExitRoots[0],
					ger.ExitRoots[1],
				}},
			tm.GetAuth(isL1))
		if err != nil {
			log.Errorf("error BuildSendClaim tx for deposit %d. Error: %v", deposit.DepositCount, err)
			return err
		}
		value := big.NewInt(0)
		if isL1 && deposit.DestinationNetwork > 1 {
			l1BridgeAddr, err := tm.GetNode(true).GetL1BridgeAddress(uint32(deposit.DestinationNetwork))
			if err != nil {
				return err
			}
			destinationL1Node, err := tm.GetDestinationL1Node(l1BridgeAddr)
			if err != nil {
				return err
			}
			fee, err := destinationL1Node.GetBridgeFee()
			if err != nil {
				return err
			}
			value = fee
		}
		if err = tm.addClaimTx(isL1, deposit.DepositCount, deposit.BlockID, tm.GetAuth(isL1).From, tx.To(), value, tx.Data(), dbTx); err != nil {
			log.Error("error adding claim tx for deposit %d. Error: %v", deposit.DepositCount, err)
			return err
		}
	}
	return nil
}

func (tm *ClaimTxManager) getNextNonce(isL1 bool, from common.Address) (uint64, error) {
	nonce, err := tm.GetNode(isL1).NonceAt(tm.ctx, from, nil)
	if err != nil {
		return 0, err
	}
	if tempNonce, found := tm.nonceCache.Get(from.Hex()); found {
		if tempNonce >= nonce {
			nonce = tempNonce + 1
		}
	}
	tm.nonceCache.Add(from.Hex(), nonce)
	return nonce, nil
}

func (tm *ClaimTxManager) addClaimTx(isL1 bool, depositCount uint, blockID uint64, from common.Address, to *common.Address, value *big.Int, data []byte, dbTx pgx.Tx) error {
	// get gas
	tx := ethereum.CallMsg{
		From:  from,
		To:    to,
		Value: value,
		Data:  data,
	}
	gas, err := tm.GetNode(isL1).EstimateGas(tm.ctx, tx)
	if err != nil {
		log.Errorf("failed to estimate gas. Ignoring tx... Error: %v, data: %s", err, common.Bytes2Hex(data))
		return nil // TODO
	}
	// get next nonce
	nonce, err := tm.getNextNonce(isL1, from)
	if err != nil {
		err := fmt.Errorf("failed to get current nonce: %w", err)
		log.Errorf(err.Error())
		return err
	}

	// create monitored tx
	mTx := ctmtypes.MonitoredTx{
		ID: depositCount, BlockID: blockID, From: from, To: to,
		Nonce: nonce, Value: value, Data: data,
		Gas: gas, Status: ctmtypes.MonitoredTxStatusCreated,
		IsL1: isL1,
	}

	// add to storage
	err = tm.storage.AddClaimTx(tm.ctx, mTx, dbTx)
	if err != nil {
		err := fmt.Errorf("failed to add tx to get monitored: %w", err)
		log.Errorf(err.Error())
		return err
	}

	return nil
}

// monitorTxs process all pending monitored tx
func (tm *ClaimTxManager) monitorTxs(ctx context.Context) (int, error) {
	// dbTx, err := tm.storage.BeginDBTransaction(tm.ctx)
	// if err != nil {
	// 	return err
	// }

	statusesFilter := []ctmtypes.MonitoredTxStatus{ctmtypes.MonitoredTxStatusCreated}
	mTxs, err := tm.storage.GetClaimTxsByStatus(ctx, statusesFilter, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get created monitored txs: %v", err)
	}

	// isResetNonce := false // it will reset the nonce in one cycle
	log.Infof("found %v monitored tx to process", len(mTxs))
	for _, mTx := range mTxs {
		mTx := mTx // force variable shadowing to avoid pointer conflicts
		mTxLog := log.WithFields("monitoredTx", mTx.ID)
		mTxLog.Infof("processing tx with nonce %d", mTx.Nonce)

		// check if any of the txs in the history was mined
		mined := false
		var receipt *types.Receipt
		hasFailedReceipts := false
		allHistoryTxMined := true
		receiptSuccessful := false

		for txHash := range mTx.History {
			mTxLog.Debugf("Checking if tx %s is mined", txHash)
			mined, receipt, err = tm.GetNode(mTx.IsL1).CheckTxWasMined(ctx, txHash)
			if err != nil {
				mTxLog.Errorf("failed to check if tx %s was mined: %v", txHash.String(), err)
				continue
			}

			// if the tx is not mined yet, check that not all the tx were mined and go to the next
			if !mined {
				// check if the tx is in the pending pool
				_, _, err = tm.GetNode(mTx.IsL1).TransactionByHash(ctx, txHash)
				if errors.Is(err, ethereum.NotFound) {
					mTxLog.Errorf("tx %s was not found in the pending pool", txHash.String())
					hasFailedReceipts = true
					continue
				} else if err != nil {
					mTxLog.Errorf("failed to get tx %s: %v", txHash.String(), err)
					continue
				}

				allHistoryTxMined = false
				continue
			}

			// if the tx was mined successfully we can break the loop and proceed
			if receipt.Status == types.ReceiptStatusSuccessful {
				mTxLog.Infof("tx %s was mined successfully", txHash.String())
				receiptSuccessful = true
				block, err := tm.GetNode(mTx.IsL1).BlockByNumber(ctx, receipt.BlockNumber)
				if err != nil {
					mTxLog.Errorf("failed to get receipt block: %v", err)
					continue
				}
				mTx.BlockID, err = tm.storage.AddBlock(ctx, &etherman.Block{
					NetworkID:   tm.networkID,
					BlockNumber: block.Number().Uint64(),
					BlockHash:   block.Hash(),
					ParentHash:  block.ParentHash(),
					ReceivedAt:  block.ReceivedAt,
				}, nil)
				if err != nil {
					mTxLog.Errorf("failed to add receipt block: %v", err)
					continue
				}
				mTx.Status = ctmtypes.MonitoredTxStatusConfirmed
				// update monitored tx changes into storage
				err = tm.storage.UpdateClaimTx(ctx, mTx, nil)
				if err != nil {
					mTxLog.Errorf("failed to update monitored tx when confirmed: %v", err)
				}
				break
			}

			// if the tx was mined but failed, we continue to consider it was not mined
			// and store the failed receipt to be used to check if nonce needs to be reviewed
			mined = false
			hasFailedReceipts = true
		}

		if receiptSuccessful {
			continue
		}

		// if the history size reaches the max history size, this means something is really wrong with
		// this Tx and we are not able to identify automatically, so we mark this as failed to let the
		// caller know something is not right and needs to be review and to avoid to monitor this
		// tx infinitely
		if allHistoryTxMined && len(mTx.History) >= maxHistorySize {
			mTx.Status = ctmtypes.MonitoredTxStatusFailed
			mTxLog.Infof("marked as failed because reached the history size limit (%d)", maxHistorySize)
			// update monitored tx changes into storage
			err = tm.storage.UpdateClaimTx(ctx, mTx, nil)
			if err != nil {
				mTxLog.Errorf("failed to update monitored tx when max history size limit reached: %v", err)
			}
			continue
		}

		// if we have failed receipts, this means at least one of the generated txs was mined
		// so maybe the current nonce was already consumed, then we need to check if there are
		// tx that were not mined yet, if so, we just need to wait, because maybe one of them
		// will get mined successfully
		if allHistoryTxMined {
			// in case of all tx were mined and none of them were mined successfully, we need to
			// review the tx information
			if hasFailedReceipts {
				mTxLog.Infof("monitored tx needs to be updated")
				err := tm.ReviewMonitoredTx(ctx, &mTx, true)
				if err != nil {
					mTxLog.Errorf("failed to review monitored tx: %v", err)
					continue
				}
			}

			// GasPrice is set here to use always the proper and most accurate value right before sending it to L2
			gasPrice, err := tm.GetNode(mTx.IsL1).SuggestGasPrice(ctx)
			if err != nil {
				mTxLog.Errorf("failed to get suggested gasPrice. Error: %v", err)
				continue
			}
			mTx.GasPrice = gasPrice

			nonce, err := tm.GetNode(mTx.IsL1).NonceAt(ctx, mTx.From, nil)
			if err != nil {
				mTxLog.Errorf("failed to get nonce. Error: %v", err)
				continue
			}
			mTx.Nonce = nonce

			// rebuild transaction
			tx := mTx.Tx()
			mTxLog.Debugf("unsigned tx created for monitored tx")

			var signedTx *types.Transaction
			// sign tx
			signedTx, err = tm.GetAuth(mTx.IsL1).Signer(mTx.From, tx)
			if err != nil {
				mTxLog.Errorf("failed to sign tx %v created from monitored tx: %v", tx.Hash().String(), err)
				continue
			}
			mTxLog.Debugf("signed tx %v created using gasPrice: %s", signedTx.Hash().String(), signedTx.GasPrice().String())

			// add tx to monitored tx history
			err = mTx.AddHistory(signedTx)
			if errors.Is(err, ctmtypes.ErrAlreadyExists) {
				mTxLog.Infof("signed tx already existed in the history")
			} else if err != nil {
				mTxLog.Errorf("failed to add signed tx to monitored tx history: %v", err)
				continue
			}

			mTxLog.Debugf("history %v for mTx", mTx.History)
			// check if the tx is already in the network, if not, send it
			_, _, err = tm.GetNode(mTx.IsL1).TransactionByHash(ctx, signedTx.Hash())
			if errors.Is(err, ethereum.NotFound) {
				err := tm.GetNode(mTx.IsL1).SendTransaction(ctx, signedTx)
				if err != nil {
					mTxLog.Errorf("failed to send tx %s to network: %v", signedTx.Hash().String(), err)
				} else {
					for {
						receipt, err := tm.GetNode(mTx.IsL1).TransactionReceipt(ctx, signedTx.Hash())
						if err == nil {
							block, err := tm.GetNode(mTx.IsL1).BlockByNumber(ctx, receipt.BlockNumber)
							if err != nil {
								mTxLog.Errorf("failed to get receipt block: %v", err)
								continue
							}
							networkID := uint(1)
							if mTx.IsL1 {
								networkID = 0
							}
							mTx.BlockID, err = tm.storage.AddBlock(ctx, &etherman.Block{
								NetworkID:   networkID,
								BlockNumber: block.Number().Uint64(),
								BlockHash:   block.Hash(),
								ParentHash:  block.ParentHash(),
								ReceivedAt:  block.ReceivedAt,
							}, nil)
							if err != nil {
								mTxLog.Errorf("failed to add receipt block: %v", err)
								continue
							}
							mTx.Status = ctmtypes.MonitoredTxStatusConfirmed
							break
						}

						if errors.Is(err, ethereum.NotFound) {
							mTxLog.Debug("Transaction not yet mined")
						} else {
							mTxLog.Debug("Receipt retrieval failed", "err", err)
						}

						time.Sleep(5 * time.Second)
					}
				}
			} else {
				mTxLog.Infof("signed tx %v already found in the network for the monitored tx: %v", signedTx.Hash().String(), err)
			}

			// update monitored tx changes into storage
			err = tm.storage.UpdateClaimTx(ctx, mTx, nil)
			if err != nil {
				mTxLog.Errorf("failed to update monitored tx: %v", err)
				continue
			}
			mTxLog.Debugf("signed tx added to the monitored tx history")

		}
	}

	// err = tm.storage.Commit(tm.ctx, nil)
	// if err != nil {
	// 	rollbackErr := tm.storage.Rollback(tm.ctx, nil)
	// 	if rollbackErr != nil {
	// 		log.Fatalf("claimtxman error rolling back state. RollbackErr: %s, err: %s", rollbackErr.Error(), err.Error())
	// 	}
	// 	log.Fatalf("UpdateClaimTx committing dbTx, err: %s", err.Error())
	// }
	return len(mTxs), nil
}

// ReviewMonitoredTx checks if tx needs to be updated
// accordingly to the current information stored and the current
// state of the blockchain
func (tm *ClaimTxManager) ReviewMonitoredTx(ctx context.Context, mTx *ctmtypes.MonitoredTx, reviewNonce bool) error {
	mTxLog := log.WithFields("monitoredTx", mTx.ID)
	mTxLog.Debug("reviewing")
	// get gas
	tx := ethereum.CallMsg{
		From:  mTx.From,
		To:    mTx.To,
		Value: mTx.Value,
		Data:  mTx.Data,
	}
	gas, err := tm.GetNode(mTx.IsL1).EstimateGas(ctx, tx)
	for i := 0; err != nil && !errors.Is(err, runtime.ErrExecutionReverted) && i < maxRetries; i++ {
		mTxLog.Warnf("error while doing gas estimation. Retrying... Error: %v, Data: %s", err, common.Bytes2Hex(tx.Data))
		time.Sleep(1 * time.Second)
		gas, err = tm.GetNode(mTx.IsL1).EstimateGas(tm.ctx, tx)
	}
	if err != nil {
		err := fmt.Errorf("failed to estimate gas. Error: %v, Data: %s", err, common.Bytes2Hex(tx.Data))
		mTxLog.Errorf(err.Error())
		return err
	}

	// check gas
	if gas > mTx.Gas {
		mTxLog.Infof("monitored tx gas updated from %v to %v", mTx.Gas, gas)
		mTx.Gas = gas
	}

	if reviewNonce {
		// check nonce
		nonce, err := tm.getNextNonce(mTx.IsL1, mTx.From)
		if err != nil {
			err := fmt.Errorf("failed to get nonce: %w", err)
			mTxLog.Errorf(err.Error())
			return err
		}
		mTxLog.Infof("monitored tx nonce updated from %v to %v", mTx.Nonce, nonce)
		mTx.Nonce = nonce
		// if nonce > mTx.Nonce {
		// 	mTxLog.Infof("monitored tx nonce updated from %v to %v", mTx.Nonce, nonce)
		// 	mTx.Nonce = nonce
		// }
	}

	return nil
}

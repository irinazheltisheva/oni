package main

import (
	"context"
	"io/ioutil"
	"math/rand"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/oni/lotus-soup/testkit"
)

type randomFile struct {
	seed      int64
	size      uint64
	localPath string
	rootCid   cid.Cid
}

func makeRandomFile(ctx context.Context, client api.FullNode, size uint64, seed int64) *randomFile {
	data := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(data)
	f, err := ioutil.TempFile("/tmp", "oni-data")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		panic(err)
	}

	// import the data to get its root cid
	res, err := client.ClientImport(ctx, api.FileRef{Path: f.Name()})
	if err != nil {
		panic(err)
	}

	// now remove the imported data; we just want the CID, but importing seems to be the easiest way to get it
	if err = client.ClientRemoveImport(ctx, res.ImportID); err != nil {
		panic(err)
	}

	return &randomFile{
		seed:      seed,
		size:      size,
		localPath: f.Name(),
		rootCid:   res.Root,
	}
}

func getPieceCommitment(ctx context.Context, client api.FullNode, minerActorAddr address.Address, file *randomFile) (*api.CommPRet, error) {
	return client.ClientCalcCommP(ctx, file.localPath, minerActorAddr)
}

func triggerMinerImport(t *testkit.TestEnvironment, ctx context.Context, file *randomFile, dealCid *cid.Cid, minerAddr address.Address) {
	t.RecordMessage("asking miner %s to import offline data for deal %s", minerAddr, dealCid)
	t.SyncClient.MustPublish(ctx, testkit.OfflineDealsTopic, testkit.ImportOfflineDataMsg{
		TargetMiner: minerAddr,
		ProposalCid: *dealCid,
		RandSeed:    file.seed,
		Size:        file.size,
	})
}

func dealsOffline(t *testkit.TestEnvironment) error {
	// Dispatch/forward non-client roles to defaults.
	if t.Role != "client" {
		return testkit.HandleDefaultRole(t)
	}

	t.RecordMessage("running client")

	cl, err := testkit.PrepareClient(t)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := cl.FullApi

	// select a random miner
	minerAddr := cl.MinerAddrs[rand.Intn(len(cl.MinerAddrs))]
	if err := client.NetConnect(ctx, minerAddr.MinerNetAddrs); err != nil {
		return err
	}

	t.RecordMessage("selected %s as the miner", minerAddr.MinerActorAddr)

	time.Sleep(2 * time.Second)

	// prepare a number of concurrent data points
	deals := t.IntParam("deals")
	// TODO make test parameters for these:
	fileSize := uint64(1600)
	price := types.NewInt(1000)
	startEpochOffset := abi.ChainEpoch(60)
	if t.IsParamSet("deal_start_epoch") {
		startEpochOffset = abi.ChainEpoch(t.IntParam("deal_start_epoch"))
	}

	// this to avoid failure to get block
	time.Sleep(2 * time.Second)

	t.RecordMessage("starting storage deals")

	var dealCids []*cid.Cid

	for i := 0; i < deals; i++ {
		seed := time.Now().Unix() * int64(i)
		file := makeRandomFile(ctx, client, fileSize, seed)

		commP, err := getPieceCommitment(ctx, client, minerAddr.MinerActorAddr, file)
		if err != nil {
			t.RecordMessage("error getting piece commitment for offline deal: %s", err)
			continue
		}

		head, err := client.ChainHead(ctx)
		if err != nil {
			return err
		}
		startEpoch := head.Height() + startEpochOffset

		deal := testkit.StartOfflineDeal(ctx, minerAddr.MinerActorAddr, client, price, startEpoch, file.rootCid, commP.Root, commP.Size)
		t.RecordMessage("started storage deal %d -> %s", i, deal)
		dealCids = append(dealCids, deal)

		triggerMinerImport(t, ctx, file, deal, minerAddr.MinerActorAddr)
	}

	pending := make(map[cid.Cid]struct{})
	for _, d := range dealCids {
		pending[*d] = struct{}{}
	}

	dealPollInterval := 2 * time.Second
	for ; len(pending) > 0; time.Sleep(dealPollInterval) {
		allDeals, err := client.ClientListDeals(ctx)
		t.RecordMessage("got %d total deals", len(allDeals))
		if err != nil {
			panic(err)
		}
		for _, di := range allDeals {
			if _, ok := pending[di.ProposalCid]; !ok {
				continue
			}

			switch di.State {
			case storagemarket.StorageDealProposalRejected:
				t.RecordMessage("deal %s rejected: %s", di.ProposalCid, di.Message)
				delete(pending, di.ProposalCid)
			case storagemarket.StorageDealFailing:
				t.RecordMessage("deal %s failed: %s", di.ProposalCid, di.Message)
				delete(pending, di.ProposalCid)
			case storagemarket.StorageDealError:
				t.RecordMessage("deal %s errored %s", di.ProposalCid, di.Message)
				delete(pending, di.ProposalCid)
			case storagemarket.StorageDealActive:
				t.RecordMessage("completed deal: %s", di)
				delete(pending, di.ProposalCid)
			}
		}
	}

	t.SyncClient.MustSignalEntry(ctx, testkit.StateStopMining)
	t.SyncClient.MustSignalAndWait(ctx, testkit.StateDone, t.TestInstanceCount)

	time.Sleep(15 * time.Second) // wait for metrics to be emitted

	return nil
}

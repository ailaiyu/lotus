package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/urfave/cli/v2"

	"github.com/filecoin-project/go-address"

	"github.com/filecoin-project/lotus/api"
	init_ "github.com/filecoin-project/lotus/chain/actors/builtin/init"
	"github.com/filecoin-project/lotus/chain/types"
	lcli "github.com/filecoin-project/lotus/cli"
	cbg "github.com/whyrusleeping/cbor-gen"
)

var staterootCmd = &cli.Command{
	Name: "stateroot",
	Subcommands: []*cli.Command{
		staterootDiffsCmd,
		staterootStatCmd,
		addressDepthStats,
	},
}

var staterootDiffsCmd = &cli.Command{
	Name:        "diffs",
	Description: "Walk down the chain and collect stats-obj changes between tipsets",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "tipset",
			Usage: "specify tipset to start from",
		},
		&cli.IntFlag{
			Name:  "count",
			Usage: "number of tipsets to count back",
			Value: 30,
		},
		&cli.BoolFlag{
			Name:  "diff",
			Usage: "compare tipset with previous",
			Value: false,
		},
	},
	Action: func(cctx *cli.Context) error {
		api, closer, err := lcli.GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}

		defer closer()
		ctx := lcli.ReqContext(cctx)

		ts, err := lcli.LoadTipSet(ctx, cctx, api)
		if err != nil {
			return err
		}

		fn := func(ts *types.TipSet) (cid.Cid, []cid.Cid) {
			blk := ts.Blocks()[0]
			strt := blk.ParentStateRoot
			cids := blk.Parents

			return strt, cids
		}

		count := cctx.Int("count")
		diff := cctx.Bool("diff")

		fmt.Printf("Height\tSize\tLinks\tObj\tBase\n")
		for i := 0; i < count; i++ {
			if ts.Height() == 0 {
				return nil
			}
			strt, cids := fn(ts)

			k := types.NewTipSetKey(cids...)
			ts, err = api.ChainGetTipSet(ctx, k)
			if err != nil {
				return err
			}

			pstrt, _ := fn(ts)

			if !diff {
				pstrt = cid.Undef
			}

			stats, err := api.ChainStatObj(ctx, strt, pstrt)
			if err != nil {
				return err
			}

			fmt.Printf("%d\t%d\t%d\t%s\t%s\n", ts.Height(), stats.Size, stats.Links, strt, pstrt)
		}

		return nil
	},
}

type statItem struct {
	Addr  address.Address
	Actor *types.Actor
	Stat  api.ObjStat
}

var staterootStatCmd = &cli.Command{
	Name:  "stat",
	Usage: "print statistics for the stateroot of a given block",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "tipset",
			Usage: "specify tipset to start from",
		},
	},
	Action: func(cctx *cli.Context) error {
		api, closer, err := lcli.GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}

		defer closer()
		ctx := lcli.ReqContext(cctx)

		ts, err := lcli.LoadTipSet(ctx, cctx, api)
		if err != nil {
			return err
		}

		var addrs []address.Address

		for _, inp := range cctx.Args().Slice() {
			a, err := address.NewFromString(inp)
			if err != nil {
				return err
			}
			addrs = append(addrs, a)
		}

		if len(addrs) == 0 {
			allActors, err := api.StateListActors(ctx, ts.Key())
			if err != nil {
				return err
			}
			addrs = allActors
		}

		var infos []statItem
		for _, a := range addrs {
			act, err := api.StateGetActor(ctx, a, ts.Key())
			if err != nil {
				return err
			}

			stat, err := api.ChainStatObj(ctx, act.Head, cid.Undef)
			if err != nil {
				return err
			}

			infos = append(infos, statItem{
				Addr:  a,
				Actor: act,
				Stat:  stat,
			})
		}

		sort.Slice(infos, func(i, j int) bool {
			return infos[i].Stat.Size > infos[j].Stat.Size
		})

		var totalActorsSize uint64
		for _, info := range infos {
			totalActorsSize += info.Stat.Size
		}

		outcap := 10
		if cctx.NArg() > outcap {
			outcap = cctx.NArg()
		}
		if len(infos) < outcap {
			outcap = len(infos)
		}

		totalStat, err := api.ChainStatObj(ctx, ts.ParentState(), cid.Undef)
		if err != nil {
			return err
		}

		fmt.Println("Total state tree size: ", totalStat.Size)
		fmt.Println("Sum of actor state size: ", totalActorsSize)
		fmt.Println("State tree structure size: ", totalStat.Size-totalActorsSize)

		fmt.Print("Addr\tType\tSize\n")
		for _, inf := range infos[:outcap] {
			cmh, err := multihash.Decode(inf.Actor.Code.Hash())
			if err != nil {
				return err
			}

			fmt.Printf("%s\t%s\t%d\n", inf.Addr, string(cmh.Digest), inf.Stat.Size)
		}
		return nil
	},
}

type apiIpldStoreApi interface {
	ChainReadObj(context.Context, cid.Cid) ([]byte, error)
}

type trackingApiStore struct {
	ctx context.Context
	api apiIpldStoreApi

	blocksRead, dataRead int
}

func (ts *trackingApiStore) Context() context.Context {
	return ts.ctx
}

func (ts *trackingApiStore) Get(ctx context.Context, c cid.Cid, out interface{}) error {
	obj, err := ts.api.ChainReadObj(ctx, c)
	if err != nil {
		return err
	}

	ts.blocksRead += 1
	ts.dataRead += len(obj)

	if err := out.(cbg.CBORUnmarshaler).UnmarshalCBOR(bytes.NewReader(obj)); err != nil {
		return err
	}
	return nil
}

func (ts *trackingApiStore) Put(ctx context.Context, v interface{}) (cid.Cid, error) {
	return cid.Undef, fmt.Errorf("Put is not implemented")
}

var addressDepthStats = &cli.Command{
	Name:        "init-stat",
	Description: "Walk down the chain and collect stats-obj changes between tipsets",
	Action: func(cctx *cli.Context) error {
		api, closer, err := lcli.GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}

		defer closer()
		ctx := lcli.ReqContext(cctx)

		head, err := api.ChainHead(ctx)
		if err != nil {
			return err
		}
		tsk := head.Key()

		actors, err := api.StateListActors(ctx, tsk)
		if err != nil {
			return err
		}
		initActor, err := api.StateGetActor(ctx, init_.Address, tsk)
		if err != nil {
			return err
		}

		store := &trackingApiStore{ctx: ctx, api: api}
		addressesResolved := 0

		printStat := func() {
			avgDepth := float64(store.blocksRead) / float64(addressesResolved)
			avgData := float64(store.dataRead) / float64(addressesResolved)

			fmt.Println("addresses: ", addressesResolved)
			fmt.Println("depth: ", avgDepth)
			fmt.Println("data: ", avgData)
			fmt.Println()
		}

		for _, actor := range actors {
			addr, err := api.StateAccountKey(ctx, actor, tsk)
			if err != nil {
				continue
			}
			switch addr.Protocol() {
			case address.BLS, address.SECP256K1:
			default:
				continue
			}
			initState, err := init_.Load(store, initActor)
			if err != nil {
				return err
			}
			addressesResolved += 1
			_, _, err = initState.ResolveAddress(addr)
			if err != nil {
				return err
			}
			if addressesResolved%1000 == 0 {
				printStat()
			}
		}

		printStat()

		return nil
	},
}

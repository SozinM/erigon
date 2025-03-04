package app

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/common/dir"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/downloader/snaptype"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcfg"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/log/v3"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/semaphore"

	"github.com/ledgerwatch/erigon/cmd/hack/tool/fromdb"
	"github.com/ledgerwatch/erigon/cmd/utils"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/ethconfig/estimate"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/debug"
	"github.com/ledgerwatch/erigon/turbo/logging"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync"
)

const ASSERT = false

func joinFlags(lists ...[]cli.Flag) (res []cli.Flag) {
	for _, list := range lists {
		res = append(res, list...)
	}
	return res
}

var snapshotCommand = cli.Command{
	Name:        "snapshots",
	Description: `Managing snapshots (historical data partitions)`,
	Subcommands: []*cli.Command{
		{
			Name:   "create",
			Action: doSnapshotCommand,
			Usage:  "Create snapshots for given range of blocks",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags: joinFlags([]cli.Flag{
				&utils.DataDirFlag,
				&SnapshotFromFlag,
				&SnapshotToFlag,
				&SnapshotSegmentSizeFlag,
			}, debug.Flags, logging.Flags),
		},
		{
			Name:   "index",
			Action: doIndicesCommand,
			Usage:  "Create all indices for snapshots",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags: joinFlags([]cli.Flag{
				&utils.DataDirFlag,
				&SnapshotFromFlag,
				&SnapshotRebuildFlag,
			}, debug.Flags, logging.Flags),
		},
		{
			Name:   "retire",
			Action: doRetireCommand,
			Usage:  "erigon snapshots uncompress a.seg | erigon snapshots compress b.seg",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags: joinFlags([]cli.Flag{
				&utils.DataDirFlag,
				&SnapshotFromFlag,
				&SnapshotToFlag,
				&SnapshotEveryFlag,
			}, debug.Flags, logging.Flags),
		},
		{
			Name:   "uncompress",
			Action: doUncompress,
			Usage:  "erigon snapshots uncompress a.seg | erigon snapshots compress b.seg",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  joinFlags([]cli.Flag{}, debug.Flags, logging.Flags),
		},
		{
			Name:   "compress",
			Action: doCompress,
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  joinFlags([]cli.Flag{&utils.DataDirFlag}, debug.Flags, logging.Flags),
		},
		{
			Name:   "ram",
			Action: doRam,
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  joinFlags([]cli.Flag{&utils.DataDirFlag}, debug.Flags, logging.Flags),
		},
		{
			Name:   "decompress_speed",
			Action: doDecompressSpeed,
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  joinFlags([]cli.Flag{&utils.DataDirFlag}, debug.Flags, logging.Flags),
		},
	},
}

var (
	SnapshotFromFlag = cli.Uint64Flag{
		Name:  "from",
		Usage: "From block number",
		Value: 0,
	}
	SnapshotToFlag = cli.Uint64Flag{
		Name:  "to",
		Usage: "To block number. Zero - means unlimited.",
		Value: 0,
	}
	SnapshotEveryFlag = cli.Uint64Flag{
		Name:  "every",
		Usage: "Do operation every N blocks",
		Value: 1_000,
	}
	SnapshotSegmentSizeFlag = cli.Uint64Flag{
		Name:  "segment.size",
		Usage: "Amount of blocks in each segment",
		Value: snaptype.Erigon2SegmentSize,
	}
	SnapshotRebuildFlag = cli.BoolFlag{
		Name:  "rebuild",
		Usage: "Force rebuild",
	}
)

func preloadFileAsync(name string) {
	go func() {
		ff, _ := os.Open(name)
		_, _ = io.CopyBuffer(io.Discard, bufio.NewReaderSize(ff, 64*1024*1024), make([]byte, 64*1024*1024))
	}()
}

func doDecompressSpeed(cliCtx *cli.Context) error {
	args := cliCtx.Args()
	if args.Len() != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args.First()

	compress.SetDecompressionTableCondensity(9)

	preloadFileAsync(f)

	decompressor, err := compress.NewDecompressor(f)
	if err != nil {
		return err
	}
	defer decompressor.Close()
	func() {
		defer decompressor.EnableReadAhead().DisableReadAhead()

		t := time.Now()
		g := decompressor.MakeGetter()
		buf := make([]byte, 0, 16*etl.BufIOSize)
		for g.HasNext() {
			buf, _ = g.Next(buf[:0])
		}
		log.Info("decompress speed", "took", time.Since(t))
	}()
	func() {
		defer decompressor.EnableReadAhead().DisableReadAhead()

		t := time.Now()
		g := decompressor.MakeGetter()
		for g.HasNext() {
			_ = g.Skip()
		}
		log.Info("decompress skip speed", "took", time.Since(t))
	}()
	return nil
}
func doRam(cliCtx *cli.Context) error {
	args := cliCtx.Args()
	if args.Len() != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args.First()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	runtime.ReadMemStats(&m)
	before := m.Alloc
	log.Info("RAM before open", "alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys))
	decompressor, err := compress.NewDecompressor(f)
	if err != nil {
		return err
	}
	defer decompressor.Close()
	runtime.ReadMemStats(&m)
	log.Info("RAM after open", "alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys), "diff", common.ByteCount(m.Alloc-before))
	return nil
}
func doIndicesCommand(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()

	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	rebuild := cliCtx.Bool(SnapshotRebuildFlag.Name)
	from := cliCtx.Uint64(SnapshotFromFlag.Name)

	chainDB := mdbx.NewMDBX(log.New()).Path(dirs.Chaindata).Readonly().MustOpen()
	defer chainDB.Close()

	dir.MustExist(dirs.SnapHistory)

	if rebuild {
		panic("not implemented")
	}
	cfg := ethconfig.NewSnapCfg(true, true, false)
	sem := semaphore.NewWeighted(int64(estimate.IndexSnapshot.Workers()))
	if err := rebuildIndices("Indexing", ctx, chainDB, cfg, dirs, from, sem); err != nil {
		log.Error("Error", "err", err)
	}
	agg, err := libstate.NewAggregator22(dirs.SnapHistory, dirs.Tmp, ethconfig.HistoryV3AggregationStep, chainDB)
	if err != nil {
		return err
	}
	err = agg.ReopenFiles()
	if err != nil {
		return err
	}
	err = agg.BuildMissedIndices(ctx, sem)
	if err != nil {
		return err
	}
	return nil
}

func doUncompress(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()
	args := cliCtx.Args()
	if args.Len() != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args.First()

	preloadFileAsync(f)

	decompressor, err := compress.NewDecompressor(f)
	if err != nil {
		return err
	}
	defer decompressor.Close()
	wr := bufio.NewWriterSize(os.Stdout, int(128*datasize.MB))
	defer wr.Flush()
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()

	var i uint
	var numBuf [binary.MaxVarintLen64]byte
	defer decompressor.EnableReadAhead().DisableReadAhead()

	g := decompressor.MakeGetter()
	buf := make([]byte, 0, 1*datasize.MB)
	for g.HasNext() {
		buf, _ = g.Next(buf[:0])
		n := binary.PutUvarint(numBuf[:], uint64(len(buf)))
		if _, err := wr.Write(numBuf[:n]); err != nil {
			return err
		}
		if _, err := wr.Write(buf); err != nil {
			return err
		}
		i++
		select {
		case <-logEvery.C:
			_, fileName := filepath.Split(decompressor.FilePath())
			progress := 100 * float64(i) / float64(decompressor.Count())
			log.Info("[uncompress] ", "progress", fmt.Sprintf("%.2f%%", progress), "file", fileName)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return nil
}
func doCompress(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()
	args := cliCtx.Args()
	if args.Len() != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args.First()
	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	c, err := compress.NewCompressor(ctx, "compress", f, dirs.Tmp, compress.MinPatternScore, estimate.CompressSnapshot.Workers(), log.LvlInfo)
	if err != nil {
		return err
	}
	defer c.Close()
	r := bufio.NewReaderSize(os.Stdin, int(128*datasize.MB))
	buf := make([]byte, 0, int(1*datasize.MB))
	var l uint64
	for l, err = binary.ReadUvarint(r); err == nil; l, err = binary.ReadUvarint(r) {
		if cap(buf) < int(l) {
			buf = make([]byte, l)
		} else {
			buf = buf[:l]
		}
		if _, err = io.ReadFull(r, buf); err != nil {
			return err
		}
		if err = c.AddWord(buf); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := c.Compress(); err != nil {
		return err
	}

	return nil
}
func doRetireCommand(cliCtx *cli.Context) error {
	defer log.Info("Retire Done")

	ctx, cancel := common.RootContext()
	defer cancel()

	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	from := cliCtx.Uint64(SnapshotFromFlag.Name)
	to := cliCtx.Uint64(SnapshotToFlag.Name)
	every := cliCtx.Uint64(SnapshotEveryFlag.Name)

	db := mdbx.NewMDBX(log.New()).Label(kv.ChainDB).Path(dirs.Chaindata).MustOpen()
	defer db.Close()

	cfg := ethconfig.NewSnapCfg(true, true, true)
	snapshots := snapshotsync.NewRoSnapshots(cfg, dirs.Snap)
	if err := snapshots.ReopenWithDB(db); err != nil {
		return err
	}

	br := snapshotsync.NewBlockRetire(estimate.CompressSnapshot.Workers(), dirs.Tmp, snapshots, db, nil, nil)

	agg, err := libstate.NewAggregator22(dirs.SnapHistory, dirs.Tmp, ethconfig.HistoryV3AggregationStep, db)
	if err != nil {
		return err
	}
	err = agg.ReopenFiles()
	if err != nil {
		return err
	}
	agg.SetWorkers(estimate.CompressSnapshot.Workers())

	log.Info("Params", "from", from, "to", to, "every", every)
	for i := from; i < to; i += every {
		if err := br.RetireBlocks(ctx, i, i+every, log.LvlInfo); err != nil {
			panic(err)
		}
		if err := db.Update(ctx, func(tx kv.RwTx) error {
			if err := rawdb.WriteSnapshots(tx, br.Snapshots().Files(), agg.Files()); err != nil {
				return err
			}
			log.Info("prune blocks from db\n")
			for j := 0; j < 10_000; j++ { // prune happens by small steps, so need many runs
				if err := br.PruneAncientBlocks(tx); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if !kvcfg.HistoryV3.FromDB(db) {
		return nil
	}

	log.Info("Prune state history")
	for i := 0; i < 1024; i++ {
		if err := db.Update(ctx, func(tx kv.RwTx) error {
			agg.SetTx(tx)
			if err = agg.Prune(ctx, ethconfig.HistoryV3AggregationStep); err != nil {
				return err
			}
			return err
		}); err != nil {
			return err
		}
	}

	log.Info("Work on state history snapshots")
	sem := semaphore.NewWeighted(int64(estimate.IndexSnapshot.Workers()))
	if err = agg.BuildMissedIndices(ctx, sem); err != nil {
		return err
	}

	if err := db.View(ctx, func(tx kv.Tx) error {
		execProgress, _ := stages.GetStageProgress(tx, stages.Execution)
		lastTxNum, err := rawdb.TxNums.Max(tx, execProgress)
		if err != nil {
			return err
		}
		agg.SetTxNum(lastTxNum)
		return nil
	}); err != nil {
		return err
	}

	log.Info("Build state history snapshots")
	if err = agg.BuildFiles(ctx, db); err != nil {
		return err
	}
	if err = agg.MergeLoop(ctx, estimate.CompressSnapshot.Workers()); err != nil {
		return err
	}

	log.Info("Prune state history")
	for i := 0; i < 1024; i++ {
		if err := db.Update(ctx, func(tx kv.RwTx) error {
			agg.SetTx(tx)
			if err = agg.Prune(ctx, ethconfig.HistoryV3AggregationStep); err != nil {
				return err
			}
			return err
		}); err != nil {
			return err
		}
	}

	return nil
}

func doSnapshotCommand(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()

	fromBlock := cliCtx.Uint64(SnapshotFromFlag.Name)
	toBlock := cliCtx.Uint64(SnapshotToFlag.Name)
	segmentSize := cliCtx.Uint64(SnapshotSegmentSizeFlag.Name)
	if segmentSize < 1000 {
		return fmt.Errorf("too small --segment.size %d", segmentSize)
	}
	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	dir.MustExist(dirs.Snap)
	dir.MustExist(dirs.SnapHistory)
	dir.MustExist(dirs.Tmp)

	db := mdbx.NewMDBX(log.New()).Label(kv.ChainDB).Path(dirs.Chaindata).MustOpen()
	defer db.Close()

	{
		if err := snapshotBlocks(ctx, db, fromBlock, toBlock, segmentSize, dirs.Snap, dirs.Tmp); err != nil {
			log.Error("Error", "err", err)
		}
		allSnapshots := snapshotsync.NewRoSnapshots(ethconfig.NewSnapCfg(true, true, true), dirs.Snap)
		if err := allSnapshots.ReopenFolder(); err != nil {
			return err
		}

		agg, err := libstate.NewAggregator22(dirs.SnapHistory, dirs.Tmp, ethconfig.HistoryV3AggregationStep, db)
		if err != nil {
			return err
		}
		err = agg.ReopenFiles()
		if err != nil {
			return err
		}
		agg.SetWorkers(estimate.CompressSnapshot.Workers())

		if err := db.Update(ctx, func(tx kv.RwTx) error {
			return rawdb.WriteSnapshots(tx, allSnapshots.Files(), agg.Files())
		}); err != nil {
			return err
		}
	}
	return nil
}

func rebuildIndices(logPrefix string, ctx context.Context, db kv.RoDB, cfg ethconfig.Snapshot, dirs datadir.Dirs, from uint64, sem *semaphore.Weighted) error {
	chainConfig := fromdb.ChainConfig(db)
	chainID, _ := uint256.FromBig(chainConfig.ChainID)

	allSnapshots := snapshotsync.NewRoSnapshots(cfg, dirs.Snap)
	if err := allSnapshots.ReopenFolder(); err != nil {
		return err
	}
	allSnapshots.LogStat()

	if err := snapshotsync.BuildMissedIndices(logPrefix, ctx, dirs, *chainID, sem); err != nil {
		return err
	}
	return nil
}

func snapshotBlocks(ctx context.Context, db kv.RoDB, fromBlock, toBlock, blocksPerFile uint64, snapDir, tmpDir string) error {
	var last uint64

	if toBlock > 0 {
		last = toBlock
	} else {
		lastChunk := func(tx kv.Tx, blocksPerFile uint64) (uint64, error) {
			c, err := tx.Cursor(kv.BlockBody)
			if err != nil {
				return 0, err
			}
			k, _, err := c.Last()
			if err != nil {
				return 0, err
			}
			last := binary.BigEndian.Uint64(k)
			if last > params.FullImmutabilityThreshold {
				last -= params.FullImmutabilityThreshold
			} else {
				last = 0
			}
			last = last - last%blocksPerFile
			return last, nil
		}

		if err := db.View(context.Background(), func(tx kv.Tx) (err error) {
			last, err = lastChunk(tx, blocksPerFile)
			return err
		}); err != nil {
			return err
		}
	}

	log.Info("Last body number", "last", last)
	if err := snapshotsync.DumpBlocks(ctx, fromBlock, last, blocksPerFile, tmpDir, snapDir, db, estimate.CompressSnapshot.Workers(), log.LvlInfo); err != nil {
		return fmt.Errorf("DumpBlocks: %w", err)
	}
	return nil
}

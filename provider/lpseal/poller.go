package lpseal

import (
	"context"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/harmony/harmonydb"
	"github.com/filecoin-project/lotus/lib/harmony/harmonytask"
	"github.com/filecoin-project/lotus/lib/promise"
)

var log = logging.Logger("lpseal")

const (
	pollerSDR = iota
	pollerTrees
	pollerPrecommitMsg
	pollerPoRep
	pollerCommitMsg

	numPollers
)

const sealPollerInterval = 10 * time.Second
const seedEpochConfidence = 3

type SealPollerAPI interface {
	StateSectorPreCommitInfo(context.Context, address.Address, abi.SectorNumber, types.TipSetKey) (*miner.SectorPreCommitOnChainInfo, error)
	StateSectorGetInfo(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tsk types.TipSetKey) (*miner.SectorOnChainInfo, error)
	ChainHead(context.Context) (*types.TipSet, error)
}

type SealPoller struct {
	db  *harmonydb.DB
	api SealPollerAPI

	pollers [numPollers]promise.Promise[harmonytask.AddTaskFunc]
}

func NewPoller(db *harmonydb.DB, api SealPollerAPI) *SealPoller {
	return &SealPoller{
		db:  db,
		api: api,
	}
}

func (s *SealPoller) RunPoller(ctx context.Context) {
	ticker := time.NewTicker(sealPollerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.poll(ctx); err != nil {
				log.Errorw("polling failed", "error", err)
			}
		}
	}
}

type pollTask struct {
	SpID         int64 `db:"sp_id"`
	SectorNumber int64 `db:"sector_number"`

	TaskSDR  *int64 `db:"task_id_sdr"`
	AfterSDR bool   `db:"after_sdr"`

	TaskTreeD  *int64 `db:"task_id_tree_d"`
	AfterTreeD bool   `db:"after_tree_d"`

	TaskTreeC  *int64 `db:"task_id_tree_c"`
	AfterTreeC bool   `db:"after_tree_c"`

	TaskTreeR  *int64 `db:"task_id_tree_r"`
	AfterTreeR bool   `db:"after_tree_r"`

	TaskPrecommitMsg  *int64 `db:"task_id_precommit_msg"`
	AfterPrecommitMsg bool   `db:"after_precommit_msg"`

	AfterPrecommitMsgSuccess bool   `db:"after_precommit_msg_success"`
	SeedEpoch                *int64 `db:"seed_epoch"`

	TaskPoRep  *int64 `db:"task_id_porep"`
	PoRepProof []byte `db:"porep_proof"`
	AfterPoRep bool   `db:"after_porep"`

	TaskCommitMsg  *int64 `db:"task_id_commit_msg"`
	AfterCommitMsg bool   `db:"after_commit_msg"`

	AfterCommitMsgSuccess bool `db:"after_commit_msg_success"`

	Failed       bool   `db:"failed"`
	FailedReason string `db:"failed_reason"`
}

func (s *SealPoller) poll(ctx context.Context) error {
	var tasks []pollTask

	err := s.db.Select(ctx, &tasks, `SELECT 
       sp_id, sector_number,
       task_id_sdr, after_sdr,
       task_id_tree_d, after_tree_d,
       task_id_tree_c, after_tree_c,
       task_id_tree_r, after_tree_r,
       task_id_precommit_msg, after_precommit_msg,
       after_precommit_msg_success, seed_epoch,
       task_id_porep, porep_proof, after_porep,
       task_id_commit_msg, after_commit_msg,
       after_commit_msg_success,
       failed, failed_reason
    FROM sectors_sdr_pipeline WHERE after_commit_msg_success != true`)
	if err != nil {
		return err
	}

	for _, task := range tasks {
		task := task
		if task.Failed {
			continue
		}

		ts, err := s.api.ChainHead(ctx)
		if err != nil {
			return xerrors.Errorf("getting chain head: %w", err)
		}

		s.pollStartSDR(ctx, task)
		s.pollStartSDRTrees(ctx, task)
		s.pollStartPrecommitMsg(ctx, task)
		s.mustPoll(s.pollPrecommitMsgLanded(ctx, task))
		s.pollStartPoRep(ctx, task, ts)
		s.pollStartCommitMsg(ctx, task)
		s.mustPoll(s.pollCommitMsgLanded(ctx, task))
	}

	return nil
}

func (s *SealPoller) pollStartSDR(ctx context.Context, task pollTask) {
	if task.TaskSDR == nil && s.pollers[pollerSDR].IsSet() {
		s.pollers[pollerSDR].Val(ctx)(func(id harmonytask.TaskID, tx *harmonydb.Tx) (shouldCommit bool, seriousError error) {
			n, err := tx.Exec(`UPDATE sectors_sdr_pipeline SET task_id_sdr = $1 WHERE sp_id = $2 AND sector_number = $3 and task_id_sdr is null`, id, task.SpID, task.SectorNumber)
			if err != nil {
				return false, xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
			}
			if n != 1 {
				return false, xerrors.Errorf("expected to update 1 row, updated %d", n)
			}

			return true, nil
		})
	}
}

func (s *SealPoller) pollStartSDRTrees(ctx context.Context, task pollTask) {
	if task.TaskTreeD == nil && task.TaskTreeC == nil && task.TaskTreeR == nil && s.pollers[pollerTrees].IsSet() && task.AfterSDR {
		s.pollers[pollerTrees].Val(ctx)(func(id harmonytask.TaskID, tx *harmonydb.Tx) (shouldCommit bool, seriousError error) {
			n, err := tx.Exec(`UPDATE sectors_sdr_pipeline SET task_id_tree_d = $1, task_id_tree_c = $1, task_id_tree_r = $1
                            WHERE sp_id = $2 AND sector_number = $3 and after_sdr = true and task_id_tree_d is null and task_id_tree_c is null and task_id_tree_r is null`, id, task.SpID, task.SectorNumber)
			if err != nil {
				return false, xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
			}
			if n != 1 {
				return false, xerrors.Errorf("expected to update 1 row, updated %d", n)
			}

			return true, nil
		})
	}
}

func (s *SealPoller) pollStartPrecommitMsg(ctx context.Context, task pollTask) {
	if task.TaskPrecommitMsg == nil && task.AfterTreeR && task.AfterTreeD {
		s.pollers[pollerPrecommitMsg].Val(ctx)(func(id harmonytask.TaskID, tx *harmonydb.Tx) (shouldCommit bool, seriousError error) {
			n, err := tx.Exec(`UPDATE sectors_sdr_pipeline SET task_id_precommit_msg = $1 WHERE sp_id = $2 AND sector_number = $3 and task_id_precommit_msg is null and after_tree_r = true and after_tree_d = true`, id, task.SpID, task.SectorNumber)
			if err != nil {
				return false, xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
			}
			if n != 1 {
				return false, xerrors.Errorf("expected to update 1 row, updated %d", n)
			}

			return true, nil
		})
	}
}

func (s *SealPoller) pollPrecommitMsgLanded(ctx context.Context, task pollTask) error {
	if task.TaskPrecommitMsg != nil && !task.AfterPrecommitMsgSuccess {
		var execResult []struct {
			ExecutedTskCID   string `db:"executed_tsk_cid"`
			ExecutedTskEpoch int64  `db:"executed_tsk_epoch"`
			ExecutedMsgCID   string `db:"executed_msg_cid"`

			ExecutedRcptExitCode int64 `db:"executed_rcpt_exitcode"`
			ExecutedRcptGasUsed  int64 `db:"executed_rcpt_gas_used"`
		}

		err := s.db.Select(ctx, &execResult, `SELECT executed_tsk_cid, executed_tsk_epoch, executed_msg_cid, executed_rcpt_exitcode, executed_rcpt_gas_used
					FROM sectors_sdr_pipeline
					JOIN message_waits ON sectors_sdr_pipeline.precommit_msg_cid = message_waits.signed_message_cid
					WHERE sp_id = $1 AND sector_number = $2 AND executed_tsk_epoch is not null`, task.SpID, task.SectorNumber)
		if err != nil {
			log.Errorw("failed to query message_waits", "error", err)
		}

		if len(execResult) > 0 {
			maddr, err := address.NewIDAddress(uint64(task.SpID))
			if err != nil {
				return err
			}

			pci, err := s.api.StateSectorPreCommitInfo(ctx, maddr, abi.SectorNumber(task.SectorNumber), types.EmptyTSK)
			if err != nil {
				return xerrors.Errorf("get precommit info: %w", err)
			}

			if pci != nil {
				randHeight := pci.PreCommitEpoch + policy.GetPreCommitChallengeDelay()

				_, err := s.db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET 
                                seed_epoch = $1, precommit_msg_tsk = $2, after_precommit_msg_success = true 
                            WHERE sp_id = $3 AND sector_number = $4 and seed_epoch is NULL`,
					randHeight, execResult[0].ExecutedTskCID, task.SpID, task.SectorNumber)
				if err != nil {
					return xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
				}
			} // todo handle missing precommit info (eg expired precommit)

		}
	}

	return nil
}

func (s *SealPoller) pollStartPoRep(ctx context.Context, task pollTask, ts *types.TipSet) {
	if s.pollers[pollerPoRep].IsSet() && task.AfterPrecommitMsgSuccess && task.SeedEpoch != nil && task.TaskPoRep == nil && ts.Height() >= abi.ChainEpoch(*task.SeedEpoch+seedEpochConfidence) {
		s.pollers[pollerPoRep].Val(ctx)(func(id harmonytask.TaskID, tx *harmonydb.Tx) (shouldCommit bool, seriousError error) {
			n, err := tx.Exec(`UPDATE sectors_sdr_pipeline SET task_id_porep = $1 WHERE sp_id = $2 AND sector_number = $3 and task_id_porep is null`, id, task.SpID, task.SectorNumber)
			if err != nil {
				return false, xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
			}
			if n != 1 {
				return false, xerrors.Errorf("expected to update 1 row, updated %d", n)
			}

			return true, nil
		})
	}
}

func (s *SealPoller) pollStartCommitMsg(ctx context.Context, task pollTask) {
	if task.AfterPoRep && len(task.PoRepProof) > 0 && task.TaskCommitMsg == nil && s.pollers[pollerCommitMsg].IsSet() {
		s.pollers[pollerCommitMsg].Val(ctx)(func(id harmonytask.TaskID, tx *harmonydb.Tx) (shouldCommit bool, seriousError error) {
			n, err := tx.Exec(`UPDATE sectors_sdr_pipeline SET task_id_commit_msg = $1 WHERE sp_id = $2 AND sector_number = $3 and task_id_commit_msg is null`, id, task.SpID, task.SectorNumber)
			if err != nil {
				return false, xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
			}
			if n != 1 {
				return false, xerrors.Errorf("expected to update 1 row, updated %d", n)
			}

			return true, nil
		})
	}
}

func (s *SealPoller) pollCommitMsgLanded(ctx context.Context, task pollTask) error {
	if task.AfterCommitMsg && !task.AfterCommitMsgSuccess && s.pollers[pollerCommitMsg].IsSet() {
		var execResult []struct {
			ExecutedTskCID   string `db:"executed_tsk_cid"`
			ExecutedTskEpoch int64  `db:"executed_tsk_epoch"`
			ExecutedMsgCID   string `db:"executed_msg_cid"`

			ExecutedRcptExitCode int64 `db:"executed_rcpt_exitcode"`
			ExecutedRcptGasUsed  int64 `db:"executed_rcpt_gas_used"`
		}

		err := s.db.Select(ctx, &execResult, `SELECT executed_tsk_cid, executed_tsk_epoch, executed_msg_cid, executed_rcpt_exitcode, executed_rcpt_gas_used
					FROM sectors_sdr_pipeline
					JOIN message_waits ON sectors_sdr_pipeline.commit_msg_cid = message_waits.signed_message_cid
					WHERE sp_id = $1 AND sector_number = $2 AND executed_tsk_epoch is not null`, task.SpID, task.SectorNumber)
		if err != nil {
			log.Errorw("failed to query message_waits", "error", err)
		}

		if len(execResult) > 0 {
			maddr, err := address.NewIDAddress(uint64(task.SpID))
			if err != nil {
				return err
			}

			si, err := s.api.StateSectorGetInfo(ctx, maddr, abi.SectorNumber(task.SectorNumber), types.EmptyTSK)
			if err != nil {
				return xerrors.Errorf("get sector info: %w", err)
			}

			if si == nil {
				log.Errorw("todo handle missing sector info (not found after cron)", "sp", task.SpID, "sector", task.SectorNumber, "exec_epoch", execResult[0].ExecutedTskEpoch, "exec_tskcid", execResult[0].ExecutedTskCID, "msg_cid", execResult[0].ExecutedMsgCID)
				// todo handdle missing sector info (not found after cron)
			} else {
				// yay!

				_, err := s.db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET
						after_commit_msg_success = true, commit_msg_tsk = $1
						WHERE sp_id = $2 AND sector_number = $3 and after_commit_msg_success = false`,
					execResult[0].ExecutedTskCID, task.SpID, task.SectorNumber)
				if err != nil {
					return xerrors.Errorf("update sectors_sdr_pipeline: %w", err)
				}
			}
		}
	}

	return nil
}

func (s *SealPoller) mustPoll(err error) {
	if err != nil {
		log.Errorw("poller operation failed", "error", err)
	}
}

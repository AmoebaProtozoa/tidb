// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/executor/internal/exec"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/sessiontxn"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/statistics/handle"
	"github.com/pingcap/tidb/statistics/handle/cache"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/dbterror/exeerrors"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

var _ exec.Executor = &AnalyzeExec{}

// AnalyzeExec represents Analyze executor.
type AnalyzeExec struct {
	exec.BaseExecutor
	tasks      []*analyzeTask
	wg         util.WaitGroupWrapper
	opts       map[ast.AnalyzeOptionType]uint64
	OptionsMap map[int64]core.V2AnalyzeOptions
}

var (
	// RandSeed is the seed for randing package.
	// It's public for test.
	RandSeed = int64(1)

	// MaxRegionSampleSize is the max sample size for one region when analyze v1 collects samples from table.
	// It's public for test.
	MaxRegionSampleSize = int64(1000)
)

const (
	maxSketchSize = 10000
)

type taskType int

const (
	colTask taskType = iota
	idxTask
	fastTask
	pkIncrementalTask
	idxIncrementalTask
)

// Next implements the Executor Next interface.
// It will collect all the sample task and run them concurrently.
func (e *AnalyzeExec) Next(ctx context.Context, _ *chunk.Chunk) error {
	statsHandle := domain.GetDomain(e.Ctx()).StatsHandle()
	infoSchema := sessiontxn.GetTxnManager(e.Ctx()).GetTxnInfoSchema()
	sessionVars := e.Ctx().GetSessionVars()

	// Filter the locked tables.
	tasks, needAnalyzeTableCnt, skippedTables, err := filterAndCollectTasks(e.tasks, statsHandle, infoSchema)
	if err != nil {
		return err
	}
	warnLockedTableMsg(sessionVars, needAnalyzeTableCnt, skippedTables)

	if len(tasks) == 0 {
		return nil
	}

	// Get the min number of goroutines for parallel execution.
	concurrency, err := getBuildStatsConcurrency(e.Ctx())
	if err != nil {
		return err
	}
	concurrency = min(len(tasks), concurrency)

	// Start workers with channel to collect results.
	taskCh := make(chan *analyzeTask, concurrency)
	resultsCh := make(chan *statistics.AnalyzeResults, len(tasks))
	for i := 0; i < concurrency; i++ {
		e.wg.Run(func() { e.analyzeWorker(taskCh, resultsCh) })
	}

	for _, task := range tasks {
		prepareV2AnalyzeJobInfo(task.colExec, false)
		AddNewAnalyzeJob(e.Ctx(), task.job)
	}
	failpoint.Inject("mockKillPendingAnalyzeJob", func() {
		dom := domain.GetDomain(e.Ctx())
		dom.SysProcTracker().KillSysProcess(dom.GetAutoAnalyzeProcID())
	})

	for _, task := range tasks {
		taskCh <- task
	}
	close(taskCh)

	// Wait all workers done and close the results channel.
	e.wg.Wait()
	close(resultsCh)

	pruneMode := variable.PartitionPruneMode(sessionVars.PartitionPruneMode.Load())
	// needGlobalStats used to indicate whether we should merge the partition-level stats to global-level stats.
	needGlobalStats := pruneMode == variable.Dynamic
	globalStatsMap := make(map[globalStatsKey]globalStatsInfo)
	err = e.handleResultsError(ctx, concurrency, needGlobalStats, globalStatsMap, resultsCh)
	for _, task := range tasks {
		if task.colExec != nil && task.colExec.memTracker != nil {
			task.colExec.memTracker.Detach()
		}
	}
	if err != nil {
		return err
	}
	failpoint.Inject("mockKillFinishedAnalyzeJob", func() {
		dom := domain.GetDomain(e.Ctx())
		dom.SysProcTracker().KillSysProcess(dom.GetAutoAnalyzeProcID())
	})

	// If we enabled dynamic prune mode, then we need to generate global stats here for partition tables.
	err = e.handleGlobalStats(ctx, needGlobalStats, globalStatsMap)
	if err != nil {
		return err
	}

	// Update analyze options to mysql.analyze_options for auto analyze.
	err = e.saveV2AnalyzeOpts()
	if err != nil {
		sessionVars.StmtCtx.AppendWarning(err)
	}

	if sessionVars.InRestrictedSQL {
		return statsHandle.Update(infoSchema)
	}

	return statsHandle.Update(infoSchema, cache.WithTableStatsByQuery())
}

// filterAndCollectTasks filters the tasks that are not locked and collects the table IDs.
func filterAndCollectTasks(tasks []*analyzeTask, statsHandle *handle.Handle, infoSchema infoschema.InfoSchema) ([]*analyzeTask, uint, []string, error) {
	var (
		filteredTasks       []*analyzeTask
		skippedTables       []string
		needAnalyzeTableCnt uint
		tids                = make([]int64, 0, len(tasks))
		// taskMap is used to collect the tasks that belong to the same table.
		// In stats v1, analyze for each index is a single task, and they have the same table id.
		taskMap = make(map[int64][]*analyzeTask, len(tasks))
	)

	for _, task := range tasks {
		tableID := getTableIDFromTask(task)
		tids = append(tids, tableID)
		taskMap[tableID] = append(taskMap[tableID], task)
	}

	// Check the locked tables in one transaction.
	lockedStatuses, err := statsHandle.QueryTablesLockedStatuses(tids...)
	if err != nil {
		return nil, 0, nil, err
	}

	for tid, isLocked := range lockedStatuses {
		if !isLocked {
			filteredTasks = append(filteredTasks, taskMap[tid]...)
			needAnalyzeTableCnt++
		} else {
			tbl, ok := infoSchema.TableByID(tid)
			if !ok {
				// Ignore this table because it may have been dropped.
				logutil.BgLogger().Warn("Unknown table ID in analyze task", zap.Int64("tid", tid))
			} else {
				skippedTables = append(skippedTables, tbl.Meta().Name.L)
			}
		}
	}

	return filteredTasks, needAnalyzeTableCnt, skippedTables, nil
}

// warnLockedTableMsg warns the locked table IDs.
func warnLockedTableMsg(sessionVars *variable.SessionVars, needAnalyzeTableCnt uint, skippedTables []string) {
	if len(skippedTables) > 0 {
		tables := strings.Join(skippedTables, ", ")
		var msg string
		if len(skippedTables) > 1 {
			msg = "skip analyze locked tables: %s"
			if needAnalyzeTableCnt > 0 {
				msg = "skip analyze locked tables: %s, other tables will be analyzed"
			}
		} else {
			msg = "skip analyze locked table: %s"
		}
		sessionVars.StmtCtx.AppendWarning(errors.Errorf(msg, tables))
	}
}

func getTableIDFromTask(task *analyzeTask) int64 {
	var tableID statistics.AnalyzeTableID

	switch task.taskType {
	case colTask:
		tableID = task.colExec.tableID
	case idxTask:
		tableID = task.idxExec.tableID
	case fastTask:
		tableID = task.fastExec.tableID
	case pkIncrementalTask:
		tableID = task.colIncrementalExec.tableID
	case idxIncrementalTask:
		tableID = task.idxIncrementalExec.tableID
	}

	return tableID.TableID
}

func (e *AnalyzeExec) saveV2AnalyzeOpts() error {
	if !variable.PersistAnalyzeOptions.Load() || len(e.OptionsMap) == 0 {
		return nil
	}
	// only to save table options if dynamic prune mode
	dynamicPrune := variable.PartitionPruneMode(e.Ctx().GetSessionVars().PartitionPruneMode.Load()) == variable.Dynamic
	toSaveMap := make(map[int64]core.V2AnalyzeOptions)
	for id, opts := range e.OptionsMap {
		if !opts.IsPartition || !dynamicPrune {
			toSaveMap[id] = opts
		}
	}
	sql := new(strings.Builder)
	sqlexec.MustFormatSQL(sql, "REPLACE INTO mysql.analyze_options (table_id,sample_num,sample_rate,buckets,topn,column_choice,column_ids) VALUES ")
	idx := 0
	for _, opts := range toSaveMap {
		sampleNum := opts.RawOpts[ast.AnalyzeOptNumSamples]
		sampleRate := float64(0)
		if val, ok := opts.RawOpts[ast.AnalyzeOptSampleRate]; ok {
			sampleRate = math.Float64frombits(val)
		}
		buckets := opts.RawOpts[ast.AnalyzeOptNumBuckets]
		topn := int64(-1)
		if val, ok := opts.RawOpts[ast.AnalyzeOptNumTopN]; ok {
			topn = int64(val)
		}
		colChoice := opts.ColChoice.String()
		colIDs := make([]string, len(opts.ColumnList))
		for i, colInfo := range opts.ColumnList {
			colIDs[i] = strconv.FormatInt(colInfo.ID, 10)
		}
		colIDStrs := strings.Join(colIDs, ",")
		sqlexec.MustFormatSQL(sql, "(%?,%?,%?,%?,%?,%?,%?)", opts.PhyTableID, sampleNum, sampleRate, buckets, topn, colChoice, colIDStrs)
		if idx < len(toSaveMap)-1 {
			sqlexec.MustFormatSQL(sql, ",")
		}
		idx++
	}
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	exec := e.Ctx().(sqlexec.RestrictedSQLExecutor)
	_, _, err := exec.ExecRestrictedSQL(ctx, nil, sql.String())
	if err != nil {
		return err
	}
	return nil
}

func recordHistoricalStats(sctx sessionctx.Context, tableID int64) error {
	statsHandle := domain.GetDomain(sctx).StatsHandle()
	historicalStatsEnabled, err := statsHandle.CheckHistoricalStatsEnable()
	if err != nil {
		return errors.Errorf("check tidb_enable_historical_stats failed: %v", err)
	}
	if !historicalStatsEnabled {
		return nil
	}
	historicalStatsWorker := domain.GetDomain(sctx).GetHistoricalStatsWorker()
	historicalStatsWorker.SendTblToDumpHistoricalStats(tableID)
	return nil
}

// handleResultsError will handle the error fetch from resultsCh and record it in log
func (e *AnalyzeExec) handleResultsError(ctx context.Context, concurrency int, needGlobalStats bool,
	globalStatsMap globalStatsMap, resultsCh <-chan *statistics.AnalyzeResults) error {
	partitionStatsConcurrency := e.Ctx().GetSessionVars().AnalyzePartitionConcurrency
	// If 'partitionStatsConcurrency' > 1, we will try to demand extra session from Domain to save Analyze results in concurrency.
	// If there is no extra session we can use, we will save analyze results in single-thread.
	if partitionStatsConcurrency > 1 {
		dom := domain.GetDomain(e.Ctx())
		subSctxs := dom.FetchAnalyzeExec(partitionStatsConcurrency)
		if len(subSctxs) > 0 {
			defer func() {
				dom.ReleaseAnalyzeExec(subSctxs)
			}()
			internalCtx := kv.WithInternalSourceType(ctx, kv.InternalTxnStats)
			err := e.handleResultsErrorWithConcurrency(internalCtx, concurrency, needGlobalStats, subSctxs, globalStatsMap, resultsCh)
			return err
		}
	}

	tableIDs := map[int64]struct{}{}

	// save analyze results in single-thread.
	statsHandle := domain.GetDomain(e.Ctx()).StatsHandle()
	panicCnt := 0
	var err error
	for panicCnt < concurrency {
		results, ok := <-resultsCh
		if !ok {
			break
		}
		if results.Err != nil {
			err = results.Err
			if isAnalyzeWorkerPanic(err) {
				panicCnt++
			} else {
				logutil.Logger(ctx).Error("analyze failed", zap.Error(err))
			}
			finishJobWithLog(e.Ctx(), results.Job, err)
			continue
		}
		handleGlobalStats(needGlobalStats, globalStatsMap, results)
		tableIDs[results.TableID.GetStatisticsID()] = struct{}{}

		if err1 := statsHandle.SaveTableStatsToStorage(results, e.Ctx().GetSessionVars().EnableAnalyzeSnapshot, handle.StatsMetaHistorySourceAnalyze); err1 != nil {
			tableID := results.TableID.TableID
			err = err1
			logutil.Logger(ctx).Error("save table stats to storage failed", zap.Error(err), zap.Int64("tableID", tableID))
			finishJobWithLog(e.Ctx(), results.Job, err)
		} else {
			finishJobWithLog(e.Ctx(), results.Job, nil)
		}
		if atomic.LoadUint32(&e.Ctx().GetSessionVars().Killed) == 1 {
			finishJobWithLog(e.Ctx(), results.Job, exeerrors.ErrQueryInterrupted)
			return errors.Trace(exeerrors.ErrQueryInterrupted)
		}
	}
	// Dump stats to historical storage.
	for tableID := range tableIDs {
		if err := recordHistoricalStats(e.Ctx(), tableID); err != nil {
			logutil.BgLogger().Error("record historical stats failed", zap.Error(err))
		}
	}

	return err
}

func (e *AnalyzeExec) handleResultsErrorWithConcurrency(ctx context.Context, statsConcurrency int, needGlobalStats bool,
	subSctxs []sessionctx.Context,
	globalStatsMap globalStatsMap, resultsCh <-chan *statistics.AnalyzeResults) error {
	partitionStatsConcurrency := len(subSctxs)

	var wg util.WaitGroupWrapper
	saveResultsCh := make(chan *statistics.AnalyzeResults, partitionStatsConcurrency)
	errCh := make(chan error, partitionStatsConcurrency)
	for i := 0; i < partitionStatsConcurrency; i++ {
		worker := newAnalyzeSaveStatsWorker(saveResultsCh, subSctxs[i], errCh, &e.Ctx().GetSessionVars().Killed)
		ctx1 := kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
		wg.Run(func() {
			worker.run(ctx1, e.Ctx().GetSessionVars().EnableAnalyzeSnapshot)
		})
	}
	tableIDs := map[int64]struct{}{}
	panicCnt := 0
	var err error
	for panicCnt < statsConcurrency {
		if atomic.LoadUint32(&e.Ctx().GetSessionVars().Killed) == 1 {
			close(saveResultsCh)
			return errors.Trace(exeerrors.ErrQueryInterrupted)
		}
		results, ok := <-resultsCh
		if !ok {
			break
		}
		if results.Err != nil {
			err = results.Err
			if isAnalyzeWorkerPanic(err) {
				panicCnt++
			} else {
				logutil.Logger(ctx).Error("analyze failed", zap.Error(err))
			}
			finishJobWithLog(e.Ctx(), results.Job, err)
			continue
		}
		handleGlobalStats(needGlobalStats, globalStatsMap, results)
		tableIDs[results.TableID.GetStatisticsID()] = struct{}{}
		saveResultsCh <- results
	}
	close(saveResultsCh)
	wg.Wait()
	close(errCh)
	if len(errCh) > 0 {
		errMsg := make([]string, 0)
		for err1 := range errCh {
			errMsg = append(errMsg, err1.Error())
		}
		err = errors.New(strings.Join(errMsg, ","))
	}
	for tableID := range tableIDs {
		// Dump stats to historical storage.
		if err := recordHistoricalStats(e.Ctx(), tableID); err != nil {
			logutil.BgLogger().Error("record historical stats failed", zap.Error(err))
		}
	}
	return err
}

func (e *AnalyzeExec) analyzeWorker(taskCh <-chan *analyzeTask, resultsCh chan<- *statistics.AnalyzeResults) {
	var task *analyzeTask
	defer func() {
		if r := recover(); r != nil {
			logutil.BgLogger().Error("analyze worker panicked", zap.Any("recover", r), zap.Stack("stack"))
			metrics.PanicCounter.WithLabelValues(metrics.LabelAnalyze).Inc()
			resultsCh <- &statistics.AnalyzeResults{
				Err: getAnalyzePanicErr(r),
				Job: task.job,
			}
		}
	}()
	for {
		var ok bool
		task, ok = <-taskCh
		if !ok {
			break
		}
		StartAnalyzeJob(e.Ctx(), task.job)
		switch task.taskType {
		case colTask:
			resultsCh <- analyzeColumnsPushDownEntry(task.colExec)
		case idxTask:
			resultsCh <- analyzeIndexPushdown(task.idxExec)
		case fastTask:
			resultsCh <- analyzeFastExec(task.fastExec)
		case pkIncrementalTask:
			resultsCh <- analyzePKIncremental(task.colIncrementalExec)
		case idxIncrementalTask:
			resultsCh <- analyzeIndexIncremental(task.idxIncrementalExec)
		}
	}
}

type analyzeTask struct {
	taskType           taskType
	idxExec            *AnalyzeIndexExec
	colExec            *AnalyzeColumnsExec
	fastExec           *AnalyzeFastExec
	idxIncrementalExec *analyzeIndexIncrementalExec
	colIncrementalExec *analyzePKIncrementalExec
	job                *statistics.AnalyzeJob
}

type baseAnalyzeExec struct {
	ctx         sessionctx.Context
	tableID     statistics.AnalyzeTableID
	concurrency int
	analyzePB   *tipb.AnalyzeReq
	opts        map[ast.AnalyzeOptionType]uint64
	job         *statistics.AnalyzeJob
	snapshot    uint64
}

// AddNewAnalyzeJob records the new analyze job.
func AddNewAnalyzeJob(ctx sessionctx.Context, job *statistics.AnalyzeJob) {
	if job == nil {
		return
	}
	var instance string
	serverInfo, err := infosync.GetServerInfo()
	if err != nil {
		logutil.BgLogger().Error("failed to get server info", zap.Error(err))
		instance = "unknown"
	} else {
		instance = net.JoinHostPort(serverInfo.IP, strconv.Itoa(int(serverInfo.Port)))
	}
	statsHandle := domain.GetDomain(ctx).StatsHandle()
	err = statsHandle.InsertAnalyzeJob(job, instance, ctx.GetSessionVars().ConnectionID)
	if err != nil {
		logutil.BgLogger().Error("failed to insert analyze job", zap.Error(err))
	}
}

// StartAnalyzeJob marks the state of the analyze job as running and sets the start time.
func StartAnalyzeJob(sctx sessionctx.Context, job *statistics.AnalyzeJob) {
	if job == nil || job.ID == nil {
		return
	}
	job.StartTime = time.Now()
	job.Progress.SetLastDumpTime(job.StartTime)
	exec := sctx.(sqlexec.RestrictedSQLExecutor)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
	const sql = "UPDATE mysql.analyze_jobs SET start_time = CONVERT_TZ(%?, '+00:00', @@TIME_ZONE), state = %? WHERE id = %?"
	_, _, err := exec.ExecRestrictedSQL(ctx, []sqlexec.OptionFuncAlias{sqlexec.ExecOptionUseSessionPool}, sql, job.StartTime.UTC().Format(types.TimeFormat), statistics.AnalyzeRunning, *job.ID)
	if err != nil {
		logutil.BgLogger().Warn("failed to update analyze job", zap.String("update", fmt.Sprintf("%s->%s", statistics.AnalyzePending, statistics.AnalyzeRunning)), zap.Error(err))
	}
}

// UpdateAnalyzeJob updates count of the processed rows when increment reaches a threshold.
func UpdateAnalyzeJob(sctx sessionctx.Context, job *statistics.AnalyzeJob, rowCount int64) {
	if job == nil || job.ID == nil {
		return
	}
	delta := job.Progress.Update(rowCount)
	if delta == 0 {
		return
	}
	exec := sctx.(sqlexec.RestrictedSQLExecutor)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
	const sql = "UPDATE mysql.analyze_jobs SET processed_rows = processed_rows + %? WHERE id = %?"
	_, _, err := exec.ExecRestrictedSQL(ctx, []sqlexec.OptionFuncAlias{sqlexec.ExecOptionUseSessionPool}, sql, delta, *job.ID)
	if err != nil {
		logutil.BgLogger().Warn("failed to update analyze job", zap.String("update", fmt.Sprintf("process %v rows", delta)), zap.Error(err))
	}
}

// FinishAnalyzeMergeJob finishes analyze merge job
func FinishAnalyzeMergeJob(sctx sessionctx.Context, job *statistics.AnalyzeJob, analyzeErr error) {
	if job == nil || job.ID == nil {
		return
	}
	job.EndTime = time.Now()
	var sql string
	var args []interface{}
	if analyzeErr != nil {
		failReason := analyzeErr.Error()
		const textMaxLength = 65535
		if len(failReason) > textMaxLength {
			failReason = failReason[:textMaxLength]
		}
		sql = "UPDATE mysql.analyze_jobs SET end_time = CONVERT_TZ(%?, '+00:00', @@TIME_ZONE), state = %?, fail_reason = %?, process_id = NULL WHERE id = %?"
		args = []interface{}{job.EndTime.UTC().Format(types.TimeFormat), statistics.AnalyzeFailed, failReason, *job.ID}
	} else {
		sql = "UPDATE mysql.analyze_jobs SET end_time = CONVERT_TZ(%?, '+00:00', @@TIME_ZONE), state = %?, process_id = NULL WHERE id = %?"
		args = []interface{}{job.EndTime.UTC().Format(types.TimeFormat), statistics.AnalyzeFinished, *job.ID}
	}
	exec := sctx.(sqlexec.RestrictedSQLExecutor)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
	_, _, err := exec.ExecRestrictedSQL(ctx, []sqlexec.OptionFuncAlias{sqlexec.ExecOptionUseSessionPool}, sql, args...)
	if err != nil {
		var state string
		if analyzeErr != nil {
			state = statistics.AnalyzeFailed
		} else {
			state = statistics.AnalyzeFinished
		}
		logutil.BgLogger().Warn("failed to update analyze job", zap.String("update", fmt.Sprintf("%s->%s", statistics.AnalyzeRunning, state)), zap.Error(err))
	}
}

// FinishAnalyzeJob updates the state of the analyze job to finished/failed according to `meetError` and sets the end time.
func FinishAnalyzeJob(sctx sessionctx.Context, job *statistics.AnalyzeJob, analyzeErr error) {
	if job == nil || job.ID == nil {
		return
	}
	job.EndTime = time.Now()
	var sql string
	var args []interface{}
	// process_id is used to see which process is running the analyze job and kill the analyze job. After the analyze job
	// is finished(or failed), process_id is useless and we set it to NULL to avoid `kill tidb process_id` wrongly.
	if analyzeErr != nil {
		failReason := analyzeErr.Error()
		const textMaxLength = 65535
		if len(failReason) > textMaxLength {
			failReason = failReason[:textMaxLength]
		}
		sql = "UPDATE mysql.analyze_jobs SET processed_rows = processed_rows + %?, end_time = CONVERT_TZ(%?, '+00:00', @@TIME_ZONE), state = %?, fail_reason = %?, process_id = NULL WHERE id = %?"
		args = []interface{}{job.Progress.GetDeltaCount(), job.EndTime.UTC().Format(types.TimeFormat), statistics.AnalyzeFailed, failReason, *job.ID}
	} else {
		sql = "UPDATE mysql.analyze_jobs SET processed_rows = processed_rows + %?, end_time = CONVERT_TZ(%?, '+00:00', @@TIME_ZONE), state = %?, process_id = NULL WHERE id = %?"
		args = []interface{}{job.Progress.GetDeltaCount(), job.EndTime.UTC().Format(types.TimeFormat), statistics.AnalyzeFinished, *job.ID}
	}
	exec := sctx.(sqlexec.RestrictedSQLExecutor)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
	_, _, err := exec.ExecRestrictedSQL(ctx, []sqlexec.OptionFuncAlias{sqlexec.ExecOptionUseSessionPool}, sql, args...)
	if err != nil {
		var state string
		if analyzeErr != nil {
			state = statistics.AnalyzeFailed
		} else {
			state = statistics.AnalyzeFinished
		}
		logutil.BgLogger().Warn("failed to update analyze job", zap.String("update", fmt.Sprintf("%s->%s", statistics.AnalyzeRunning, state)), zap.Error(err))
	}
}

func finishJobWithLog(sctx sessionctx.Context, job *statistics.AnalyzeJob, analyzeErr error) {
	FinishAnalyzeJob(sctx, job, analyzeErr)
	if job != nil {
		var state string
		if analyzeErr != nil {
			state = statistics.AnalyzeFailed
		} else {
			state = statistics.AnalyzeFinished
		}
		logutil.BgLogger().Info(fmt.Sprintf("analyze table `%s`.`%s` has %s", job.DBName, job.TableName, state),
			zap.String("partition", job.PartitionName),
			zap.String("job info", job.JobInfo),
			zap.Time("start time", job.StartTime),
			zap.Time("end time", job.EndTime),
			zap.String("cost", job.EndTime.Sub(job.StartTime).String()),
			zap.String("sample rate reason", job.SampleRateReason))
	}
}

func handleGlobalStats(needGlobalStats bool, globalStatsMap globalStatsMap, results *statistics.AnalyzeResults) {
	if results.TableID.IsPartitionTable() && needGlobalStats {
		for _, result := range results.Ars {
			if result.IsIndex == 0 {
				// If it does not belong to the statistics of index, we need to set it to -1 to distinguish.
				globalStatsID := globalStatsKey{tableID: results.TableID.TableID, indexID: int64(-1)}
				histIDs := make([]int64, 0, len(result.Hist))
				for _, hg := range result.Hist {
					// It's normal virtual column, skip.
					if hg == nil {
						continue
					}
					histIDs = append(histIDs, hg.ID)
				}
				globalStatsMap[globalStatsID] = globalStatsInfo{isIndex: result.IsIndex, histIDs: histIDs, statsVersion: results.StatsVer}
			} else {
				for _, hg := range result.Hist {
					globalStatsID := globalStatsKey{tableID: results.TableID.TableID, indexID: hg.ID}
					globalStatsMap[globalStatsID] = globalStatsInfo{isIndex: result.IsIndex, histIDs: []int64{hg.ID}, statsVersion: results.StatsVer}
				}
			}
		}
	}
}

// Copyright 2023 PingCAP, Inc.
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

package dispatcher_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ngaut/pools"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/disttask/framework/dispatcher"
	"github.com/pingcap/tidb/disttask/framework/proto"
	"github.com/pingcap/tidb/disttask/framework/storage"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/util"
)

var (
	_ dispatcher.Dispatcher = (*testDispatcher)(nil)
	_ dispatcher.Dispatcher = (*numberExampleDispatcher)(nil)
)

const (
	taskTypeExample = "task_example"
	subtaskCnt      = 3
)

type testDispatcher struct{}

func (*testDispatcher) OnTick(_ context.Context, _ *proto.Task) {
}

func (*testDispatcher) OnNextStage(_ context.Context, _ dispatcher.TaskHandle, task *proto.Task) (metas [][]byte, err error) {
	return nil, nil
}

func (*testDispatcher) OnErrStage(_ context.Context, _ dispatcher.TaskHandle, _ *proto.Task, _ []error) (meta []byte, err error) {
	return nil, nil
}

var mockedAllServerInfos = []*infosync.ServerInfo{}

func (*testDispatcher) GetEligibleInstances(_ context.Context, _ *proto.Task) ([]*infosync.ServerInfo, error) {
	return mockedAllServerInfos, nil
}

func (*testDispatcher) IsRetryableErr(error) bool {
	return true
}

type numberExampleDispatcher struct{}

func (*numberExampleDispatcher) OnTick(_ context.Context, _ *proto.Task) {
}

func (n *numberExampleDispatcher) OnNextStage(_ context.Context, _ dispatcher.TaskHandle, task *proto.Task) (metas [][]byte, err error) {
	if task.State == proto.TaskStatePending {
		task.Step = proto.StepInit
	}
	switch task.Step {
	case proto.StepInit:
		task.Step = proto.StepOne
		for i := 0; i < subtaskCnt; i++ {
			metas = append(metas, []byte{'1'})
		}
		logutil.BgLogger().Info("progress step init")
	case proto.StepOne:
		logutil.BgLogger().Info("progress step one")
		return nil, nil
	default:
		return nil, errors.New("unknown step")
	}
	return metas, nil
}

func (n *numberExampleDispatcher) OnErrStage(_ context.Context, _ dispatcher.TaskHandle, _ *proto.Task, _ []error) (meta []byte, err error) {
	// Don't handle not.
	return nil, nil
}

func (*numberExampleDispatcher) GetEligibleInstances(ctx context.Context, _ *proto.Task) ([]*infosync.ServerInfo, error) {
	return dispatcher.GenerateSchedulerNodes(ctx)
}

func (*numberExampleDispatcher) IsRetryableErr(error) bool {
	return true
}

func MockDispatcherManager(t *testing.T, pool *pools.ResourcePool) (*dispatcher.Manager, *storage.TaskManager) {
	ctx := context.WithValue(context.Background(), "etcd", true)
	mgr := storage.NewTaskManager(util.WithInternalSourceType(ctx, "taskManager"), pool)
	storage.SetTaskManager(mgr)
	dsp, err := dispatcher.NewManager(util.WithInternalSourceType(ctx, "dispatcher"), mgr, "host:port")
	require.NoError(t, err)
	dispatcher.RegisterTaskDispatcher(proto.TaskTypeExample, &testDispatcher{})
	return dsp, mgr
}

func deleteTasks(t *testing.T, store kv.Storage, taskID int64) {
	tk := testkit.NewTestKit(t, store)
	tk.MustExec(fmt.Sprintf("delete from mysql.tidb_global_task where id = %d", taskID))
}

func TestGetInstance(t *testing.T) {
	ctx := context.Background()
	store := testkit.CreateMockStore(t)
	gtk := testkit.NewTestKit(t, store)
	pool := pools.NewResourcePool(func() (pools.Resource, error) {
		return gtk.Session(), nil
	}, 1, 1, time.Second)
	defer pool.Close()

	dspManager, mgr := MockDispatcherManager(t, pool)
	// test no server
	task := &proto.Task{ID: 1, Type: proto.TaskTypeExample}
	dsp, err := dspManager.MockDispatcher(task)
	require.NoError(t, err)
	instanceIDs, err := dsp.GetAllSchedulerIDs(ctx, task)
	require.Lenf(t, instanceIDs, 0, "GetAllSchedulerIDs when there's no subtask")
	require.NoError(t, err)

	// test 2 servers
	// server ids: uuid0, uuid1
	// subtask instance ids: nil
	uuids := []string{"ddl_id_1", "ddl_id_2"}
	serverIDs := []string{"10.123.124.10:32457", "[ABCD:EF01:2345:6789:ABCD:EF01:2345:6789]:65535"}

	mockedAllServerInfos = []*infosync.ServerInfo{
		{
			ID:   uuids[0],
			IP:   "10.123.124.10",
			Port: 32457,
		},
		{
			ID:   uuids[1],
			IP:   "ABCD:EF01:2345:6789:ABCD:EF01:2345:6789",
			Port: 65535,
		},
	}
	instanceIDs, err = dsp.GetAllSchedulerIDs(ctx, task)
	require.Lenf(t, instanceIDs, 0, "GetAllSchedulerIDs")
	require.NoError(t, err)

	// server ids: uuid0, uuid1
	// subtask instance ids: uuid1
	subtask := &proto.Subtask{
		Type:        proto.TaskTypeExample,
		TaskID:      task.ID,
		SchedulerID: serverIDs[1],
	}
	err = mgr.AddNewSubTask(task.ID, proto.StepInit, subtask.SchedulerID, nil, subtask.Type, true)
	require.NoError(t, err)
	instanceIDs, err = dsp.GetAllSchedulerIDs(ctx, task)
	require.NoError(t, err)
	require.Equal(t, []string{serverIDs[1]}, instanceIDs)
	// server ids: uuid0, uuid1
	// subtask instance ids: uuid0, uuid1
	subtask = &proto.Subtask{
		Type:        proto.TaskTypeExample,
		TaskID:      task.ID,
		SchedulerID: serverIDs[0],
	}
	err = mgr.AddNewSubTask(task.ID, proto.StepInit, subtask.SchedulerID, nil, subtask.Type, true)
	require.NoError(t, err)
	instanceIDs, err = dsp.GetAllSchedulerIDs(ctx, task)
	require.NoError(t, err)
	require.Len(t, instanceIDs, len(serverIDs))
	require.ElementsMatch(t, instanceIDs, serverIDs)
}

func checkDispatch(t *testing.T, taskCnt int, isSucc bool, isCancel bool) {
	dispatcher.RegisterTaskDispatcher(taskTypeExample, &numberExampleDispatcher{})
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/domain/MockDisableDistTask", "return(true)"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/domain/MockDisableDistTask"))
	}()
	// test DispatchTaskLoop
	// test parallelism control
	var originalConcurrency int
	if taskCnt == 1 {
		originalConcurrency = dispatcher.DefaultDispatchConcurrency
		dispatcher.DefaultDispatchConcurrency = 1
	}

	store := testkit.CreateMockStore(t)
	gtk := testkit.NewTestKit(t, store)
	pool := pools.NewResourcePool(func() (pools.Resource, error) {
		return gtk.Session(), nil
	}, 1, 1, time.Second)
	defer pool.Close()

	dsp, mgr := MockDispatcherManager(t, pool)
	dsp.Start()
	defer func() {
		dsp.Stop()
		// make data race happy
		if taskCnt == 1 {
			dispatcher.DefaultDispatchConcurrency = originalConcurrency
		}
	}()
	// 3s
	cnt := 60
	checkGetRunningTaskCnt := func(expected int) {
		require.Eventually(t, func() bool {
			return dsp.GetRunningTaskCnt() == expected
		}, time.Second, 50*time.Millisecond)
	}

	checkTaskRunningCnt := func() []*proto.Task {
		var tasks []*proto.Task
		require.Eventually(t, func() bool {
			var err error
			tasks, err = mgr.GetGlobalTasksInStates(proto.TaskStateRunning)
			require.NoError(t, err)
			return len(tasks) == taskCnt
		}, time.Second, 50*time.Millisecond)
		return tasks
	}

	// Mock add tasks.
	taskIDs := make([]int64, 0, taskCnt)
	for i := 0; i < taskCnt; i++ {
		taskID, err := mgr.AddNewGlobalTask(fmt.Sprintf("%d", i), taskTypeExample, 0, nil)
		require.NoError(t, err)
		taskIDs = append(taskIDs, taskID)
	}
	// test OnNextStage.
	checkGetRunningTaskCnt(taskCnt)
	tasks := checkTaskRunningCnt()
	for i, taskID := range taskIDs {
		require.Equal(t, int64(i+1), tasks[i].ID)
		subtasks, err := mgr.GetSubtaskInStatesCnt(taskID, proto.TaskStatePending)
		require.NoError(t, err)
		require.Equal(t, int64(subtaskCnt), subtasks, fmt.Sprintf("num:%d", i))
	}
	// test parallelism control
	if taskCnt == 1 {
		taskID, err := mgr.AddNewGlobalTask(fmt.Sprintf("%d", taskCnt), taskTypeExample, 0, nil)
		require.NoError(t, err)
		checkGetRunningTaskCnt(taskCnt)
		// Clean the task.
		deleteTasks(t, store, taskID)
		dsp.DelRunningTask(taskID)
	}

	// test DetectTaskLoop
	checkGetTaskState := func(expectedState string) {
		for i := 0; i < cnt; i++ {
			tasks, err := mgr.GetGlobalTasksInStates(expectedState)
			require.NoError(t, err)
			if len(tasks) == taskCnt {
				break
			}
			time.Sleep(time.Millisecond * 50)
		}
	}
	// Test all subtasks are successful.
	var err error
	if isSucc {
		// Mock subtasks succeed.
		for i := 1; i <= subtaskCnt*taskCnt; i++ {
			err = mgr.UpdateSubtaskStateAndError(int64(i), proto.TaskStateSucceed, nil)
			require.NoError(t, err)
		}
		checkGetTaskState(proto.TaskStateSucceed)
		require.Len(t, tasks, taskCnt)

		checkGetRunningTaskCnt(0)
		return
	}

	if isCancel {
		for i := 1; i <= taskCnt; i++ {
			err = mgr.CancelGlobalTask(int64(i))
			require.NoError(t, err)
		}
	} else {
		// Test each task has a subtask failed.
		require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/disttask/framework/storage/MockUpdateTaskErr", "1*return(true)"))
		defer func() {
			require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/disttask/framework/storage/MockUpdateTaskErr"))
		}()
		// Mock a subtask fails.
		for i := 1; i <= subtaskCnt*taskCnt; i += subtaskCnt {
			err = mgr.UpdateSubtaskStateAndError(int64(i), proto.TaskStateFailed, nil)
			require.NoError(t, err)
		}
	}

	checkGetTaskState(proto.TaskStateReverting)
	require.Len(t, tasks, taskCnt)
	// Mock all subtask reverted.
	start := subtaskCnt * taskCnt
	for i := start; i <= start+subtaskCnt*taskCnt; i++ {
		err = mgr.UpdateSubtaskStateAndError(int64(i), proto.TaskStateReverted, nil)
		require.NoError(t, err)
	}
	checkGetTaskState(proto.TaskStateReverted)
	require.Len(t, tasks, taskCnt)
	checkGetRunningTaskCnt(0)
}

func TestSimple(t *testing.T) {
	checkDispatch(t, 1, true, false)
}

func TestSimpleErrStage(t *testing.T) {
	checkDispatch(t, 1, false, false)
}

func TestSimpleCancel(t *testing.T) {
	checkDispatch(t, 1, false, true)
}

func TestParallel(t *testing.T) {
	checkDispatch(t, 3, true, false)
}

func TestParallelErrStage(t *testing.T) {
	checkDispatch(t, 3, false, false)
}

func TestParallelCancel(t *testing.T) {
	checkDispatch(t, 3, false, true)
}

func TestVerifyTaskStateTransform(t *testing.T) {
	testCases := []struct {
		oldState string
		newState string
		expect   bool
	}{
		{proto.TaskStateRunning, proto.TaskStateRunning, true},
		{proto.TaskStatePending, proto.TaskStateRunning, true},
		{proto.TaskStatePending, proto.TaskStateReverting, false},
		{proto.TaskStateRunning, proto.TaskStateReverting, true},
		{proto.TaskStateReverting, proto.TaskStateReverted, true},
		{proto.TaskStateReverting, proto.TaskStateSucceed, false},
		{proto.TaskStateRunning, proto.TaskStatePausing, true},
		{proto.TaskStateRunning, proto.TaskStateResuming, false},
		{proto.TaskStateCancelling, proto.TaskStateRunning, false},
		{proto.TaskStateCanceled, proto.TaskStateRunning, false},
	}
	for _, tc := range testCases {
		require.Equal(t, tc.expect, dispatcher.VerifyTaskStateTransform(tc.oldState, tc.newState))
	}
}

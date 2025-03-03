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

package handle

import (
	"context"

	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/statistics/handle/lockstats"
	"github.com/pingcap/tidb/util/sqlexec"
)

// AddLockedTables add locked tables id to store.
// - tids: table ids of which will be locked.
// - pids: partition ids of which will be locked.
// - tables: table names of which will be locked.
// Return the message of skipped tables and error.
func (h *Handle) AddLockedTables(tids []int64, pids []int64, tables []*ast.TableName) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return lockstats.AddLockedTables(h.mu.ctx.(sqlexec.SQLExecutor), tids, pids, tables)
}

// RemoveLockedTables remove tables from table locked array.
// - tids: table ids of which will be unlocked.
// - pids: partition ids of which will be unlocked.
// - tables: table names of which will be unlocked.
// Return the message of skipped tables and error.
func (h *Handle) RemoveLockedTables(tids []int64, pids []int64, tables []*ast.TableName) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return lockstats.RemoveLockedTables(h.mu.ctx.(sqlexec.SQLExecutor), tids, pids, tables)
}

// QueryTablesLockedStatuses query whether table is locked in handle with Handle.Mutex.
// Note: This function query locked tables from store, so please try to batch the query.
func (h *Handle) QueryTablesLockedStatuses(tableIDs ...int64) (map[int64]bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.queryTablesLockedStatuses(tableIDs...)
}

// queryTablesLockedStatuses query whether table is locked in handle without Handle.Mutex
// Note: This function query locked tables from store, so please try to batch the query.
func (h *Handle) queryTablesLockedStatuses(tableIDs ...int64) (map[int64]bool, error) {
	tableLocked, err := h.queryLockedTablesWithoutLock()
	if err != nil {
		return nil, err
	}
	return lockstats.GetTablesLockedStatuses(tableLocked, tableIDs...), nil
}

// queryLockedTablesWithoutLock query locked tables from store without Handle.Mutex.
func (h *Handle) queryLockedTablesWithoutLock() (map[int64]struct{}, error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
	exec := h.mu.ctx.(sqlexec.SQLExecutor)

	return lockstats.QueryLockedTables(ctx, exec)
}

// GetTableLockedAndClearForTest for unit test only
func (h *Handle) GetTableLockedAndClearForTest() (map[int64]struct{}, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.queryLockedTablesWithoutLock()
}

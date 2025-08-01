// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package importv2

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/allocator"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache"
	"github.com/milvus-io/milvus/internal/flushcommon/syncmgr"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/importutilv2/binlog"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/proto/internalpb"
	"github.com/milvus-io/milvus/pkg/v2/util/conc"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

type L0ImportTask struct {
	*datapb.ImportTaskV2
	ctx          context.Context
	cancel       context.CancelFunc
	segmentsInfo map[int64]*datapb.ImportSegmentInfo
	req          *datapb.ImportRequest

	allocator  allocator.Interface
	manager    TaskManager
	syncMgr    syncmgr.SyncManager
	cm         storage.ChunkManager
	metaCaches map[string]metacache.MetaCache
}

func NewL0ImportTask(req *datapb.ImportRequest,
	manager TaskManager,
	syncMgr syncmgr.SyncManager,
	cm storage.ChunkManager,
) Task {
	ctx, cancel := context.WithCancel(context.Background())
	// Allocator for autoIDs and logIDs.
	alloc := allocator.NewLocalAllocator(req.GetIDRange().GetBegin(), req.GetIDRange().GetEnd())
	task := &L0ImportTask{
		ImportTaskV2: &datapb.ImportTaskV2{
			JobID:        req.GetJobID(),
			TaskID:       req.GetTaskID(),
			CollectionID: req.GetCollectionID(),
			State:        datapb.ImportTaskStateV2_Pending,
		},
		ctx:          ctx,
		cancel:       cancel,
		segmentsInfo: make(map[int64]*datapb.ImportSegmentInfo),
		req:          req,
		allocator:    alloc,
		manager:      manager,
		syncMgr:      syncMgr,
		cm:           cm,
	}
	task.metaCaches = NewMetaCache(req)
	return task
}

func (t *L0ImportTask) GetType() TaskType {
	return L0ImportTaskType
}

func (t *L0ImportTask) GetPartitionIDs() []int64 {
	return t.req.GetPartitionIDs()
}

func (t *L0ImportTask) GetVchannels() []string {
	return t.req.GetVchannels()
}

func (t *L0ImportTask) GetSchema() *schemapb.CollectionSchema {
	return t.req.GetSchema()
}

func (t *L0ImportTask) GetSlots() int64 {
	return t.req.GetTaskSlot()
}

// L0 import task buffer size is fixed
func (t *L0ImportTask) GetBufferSize() int64 {
	return paramtable.Get().DataNodeCfg.ImportBaseBufferSize.GetAsInt64()
}

func (t *L0ImportTask) Cancel() {
	t.cancel()
}

func (t *L0ImportTask) GetSegmentsInfo() []*datapb.ImportSegmentInfo {
	return lo.Values(t.segmentsInfo)
}

func (t *L0ImportTask) Clone() Task {
	ctx, cancel := context.WithCancel(t.ctx)
	infos := make(map[int64]*datapb.ImportSegmentInfo)
	for id, info := range t.segmentsInfo {
		infos[id] = typeutil.Clone(info)
	}
	return &L0ImportTask{
		ImportTaskV2: typeutil.Clone(t.ImportTaskV2),
		ctx:          ctx,
		cancel:       cancel,
		segmentsInfo: infos,
		req:          t.req,
		allocator:    t.allocator,
		manager:      t.manager,
		syncMgr:      t.syncMgr,
		cm:           t.cm,
		metaCaches:   t.metaCaches,
	}
}

func (t *L0ImportTask) Execute() []*conc.Future[any] {
	bufferSize := int(t.GetBufferSize())
	log.Info("start to import l0", WrapLogFields(t,
		zap.Int("bufferSize", bufferSize),
		zap.Int64("taskSlot", t.GetSlots()),
		zap.Any("files", t.req.GetFiles()),
		zap.Any("schema", t.GetSchema()),
	)...)
	t.manager.Update(t.GetTaskID(), UpdateState(datapb.ImportTaskStateV2_InProgress))

	req := t.req

	fn := func(file *internalpb.ImportFile) (err error) {
		defer func() {
			if err != nil {
				var reason string = err.Error()
				if len(t.req.GetFiles()) == 1 {
					reason = fmt.Sprintf("error: %v, file: %s", err, t.req.GetFiles()[0].String())
				}
				log.Warn("l0 import task execute failed", WrapLogFields(t, zap.Any("file", t.req.GetFiles()), zap.String("err", reason))...)
				t.manager.Update(t.GetTaskID(), UpdateState(datapb.ImportTaskStateV2_Failed), UpdateReason(reason))
			}
		}()

		var pkField *schemapb.FieldSchema
		pkField, err = typeutil.GetPrimaryFieldSchema(t.GetSchema())
		if err != nil {
			return
		}
		var reader binlog.L0Reader
		reader, err = binlog.NewL0Reader(t.ctx, t.cm, pkField, file, bufferSize)
		if err != nil {
			return
		}
		start := time.Now()
		err = t.importL0(reader)
		if err != nil {
			return
		}
		log.Info("l0 import done", WrapLogFields(t,
			zap.Strings("l0 prefix", file.GetPaths()),
			zap.Duration("dur", time.Since(start)))...)
		return nil
	}

	futures := make([]*conc.Future[any], 0, len(req.GetFiles()))
	for _, file := range req.GetFiles() {
		file := file
		f := GetExecPool().Submit(func() (any, error) {
			err := fn(file)
			return err, err
		})
		futures = append(futures, f)
	}
	return futures
}

func (t *L0ImportTask) importL0(reader binlog.L0Reader) error {
	syncFutures := make([]*conc.Future[struct{}], 0)
	syncTasks := make([]syncmgr.Task, 0)
	for {
		data, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		delData, err := HashDeleteData(t, data)
		if err != nil {
			return err
		}
		fs, sts, err := t.syncDelete(delData)
		if err != nil {
			return err
		}
		syncFutures = append(syncFutures, fs...)
		syncTasks = append(syncTasks, sts...)
	}
	err := conc.AwaitAll(syncFutures...)
	if err != nil {
		return err
	}
	for _, syncTask := range syncTasks {
		segmentInfo, err := NewImportSegmentInfo(syncTask, t.metaCaches)
		if err != nil {
			return err
		}
		t.manager.Update(t.GetTaskID(), UpdateSegmentInfo(segmentInfo))
		log.Info("sync l0 data done", WrapLogFields(t, zap.Any("segmentInfo", segmentInfo))...)
	}
	return nil
}

func (t *L0ImportTask) syncDelete(delData []*storage.DeleteData) ([]*conc.Future[struct{}], []syncmgr.Task, error) {
	log.Ctx(context.TODO()).Info("start to sync l0 delete data", WrapLogFields(t)...)
	futures := make([]*conc.Future[struct{}], 0)
	syncTasks := make([]syncmgr.Task, 0)
	for channelIdx, data := range delData {
		channel := t.GetVchannels()[channelIdx]
		if data.RowCount == 0 {
			continue
		}
		partitionID := t.GetPartitionIDs()[0]
		segmentID, err := PickSegment(t.req.GetRequestSegments(), channel, partitionID)
		if err != nil {
			return nil, nil, err
		}
		syncTask, err := NewSyncTask(t.ctx, t.allocator, t.metaCaches, t.req.GetTs(),
			segmentID, partitionID, t.GetCollectionID(), channel, nil, data,
			nil, t.req.GetStorageVersion(), t.req.GetStorageConfig())
		if err != nil {
			return nil, nil, err
		}
		future, err := t.syncMgr.SyncDataWithChunkManager(t.ctx, syncTask, t.cm)
		if err != nil {
			log.Ctx(context.TODO()).Error("failed to sync l0 delete data", WrapLogFields(t, zap.Error(err))...)
			return nil, nil, err
		}
		futures = append(futures, future)
		syncTasks = append(syncTasks, syncTask)
	}
	return futures, syncTasks, nil
}

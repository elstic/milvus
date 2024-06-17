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
	"github.com/golang/protobuf/proto"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/importutilv2"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/conc"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
)

type PreImportTask struct {
	*datapb.PreImportTask
	ctx          context.Context
	cancel       context.CancelFunc
	partitionIDs []int64
	vchannels    []string
	schema       *schemapb.CollectionSchema
	options      []*commonpb.KeyValuePair

	manager TaskManager
	cm      storage.ChunkManager
}

func NewPreImportTask(req *datapb.PreImportRequest,
	manager TaskManager,
	cm storage.ChunkManager,
) Task {
	fileStats := lo.Map(req.GetImportFiles(), func(file *internalpb.ImportFile, _ int) *datapb.ImportFileStats {
		return &datapb.ImportFileStats{
			ImportFile: file,
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	// During binlog import, even if the primary key's autoID is set to true,
	// the primary key from the binlog should be used instead of being reassigned.
	if importutilv2.IsBackup(req.GetOptions()) {
		UnsetAutoID(req.GetSchema())
	}
	return &PreImportTask{
		PreImportTask: &datapb.PreImportTask{
			JobID:        req.GetJobID(),
			TaskID:       req.GetTaskID(),
			CollectionID: req.GetCollectionID(),
			State:        datapb.ImportTaskStateV2_Pending,
			FileStats:    fileStats,
		},
		ctx:          ctx,
		cancel:       cancel,
		partitionIDs: req.GetPartitionIDs(),
		vchannels:    req.GetVchannels(),
		schema:       req.GetSchema(),
		options:      req.GetOptions(),
		manager:      manager,
		cm:           cm,
	}
}

func (p *PreImportTask) GetPartitionIDs() []int64 {
	return p.partitionIDs
}

func (p *PreImportTask) GetVchannels() []string {
	return p.vchannels
}

func (p *PreImportTask) GetType() TaskType {
	return PreImportTaskType
}

func (p *PreImportTask) GetSchema() *schemapb.CollectionSchema {
	return p.schema
}

func (p *PreImportTask) Cancel() {
	p.cancel()
}

func (p *PreImportTask) Clone() Task {
	ctx, cancel := context.WithCancel(p.ctx)
	return &PreImportTask{
		PreImportTask: proto.Clone(p.PreImportTask).(*datapb.PreImportTask),
		ctx:           ctx,
		cancel:        cancel,
		partitionIDs:  p.GetPartitionIDs(),
		vchannels:     p.GetVchannels(),
		schema:        p.GetSchema(),
		options:       p.options,
	}
}

func (p *PreImportTask) Execute() []*conc.Future[any] {
	bufferSize := paramtable.Get().DataNodeCfg.ReadBufferSizeInMB.GetAsInt() * 1024 * 1024
	log.Info("start to preimport", WrapLogFields(p,
		zap.Int("bufferSize", bufferSize),
		zap.Any("schema", p.GetSchema()))...)
	p.manager.Update(p.GetTaskID(), UpdateState(datapb.ImportTaskStateV2_InProgress))
	files := lo.Map(p.GetFileStats(),
		func(fileStat *datapb.ImportFileStats, _ int) *internalpb.ImportFile {
			return fileStat.GetImportFile()
		})

	fn := func(i int, file *internalpb.ImportFile) error {
		reader, err := importutilv2.NewReader(p.ctx, p.cm, p.GetSchema(), file, p.options, bufferSize)
		if err != nil {
			log.Warn("new reader failed", WrapLogFields(p, zap.String("file", file.String()), zap.Error(err))...)
			p.manager.Update(p.GetTaskID(), UpdateState(datapb.ImportTaskStateV2_Failed), UpdateReason(err.Error()))
			return err
		}
		defer reader.Close()
		start := time.Now()
		err = p.readFileStat(reader, p, i)
		if err != nil {
			log.Warn("preimport failed", WrapLogFields(p, zap.String("file", file.String()), zap.Error(err))...)
			p.manager.Update(p.GetTaskID(), UpdateState(datapb.ImportTaskStateV2_Failed), UpdateReason(err.Error()))
			return err
		}
		log.Info("read file stat done", WrapLogFields(p, zap.Strings("files", file.GetPaths()),
			zap.Duration("dur", time.Since(start)))...)
		return nil
	}

	futures := make([]*conc.Future[any], 0, len(files))
	for i, file := range files {
		i := i
		file := file
		f := GetExecPool().Submit(func() (any, error) {
			err := fn(i, file)
			return err, err
		})
		futures = append(futures, f)
	}
	return futures
}

func (p *PreImportTask) readFileStat(reader importutilv2.Reader, task Task, fileIdx int) error {
	fileSize, err := reader.Size()
	if err != nil {
		return err
	}
	maxSize := paramtable.Get().DataNodeCfg.MaxImportFileSizeInGB.GetAsFloat() * 1024 * 1024 * 1024
	if fileSize > int64(maxSize) {
		return errors.New(fmt.Sprintf(
			"The import file size has reached the maximum limit allowed for importing, "+
				"fileSize=%d, maxSize=%d", fileSize, int64(maxSize)))
	}

	totalRows := 0
	totalSize := 0
	hashedStats := make(map[string]*datapb.PartitionImportStats)
	for {
		data, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		err = CheckRowsEqual(task.GetSchema(), data)
		if err != nil {
			return err
		}
		rowsCount, err := GetRowsStats(task, data)
		if err != nil {
			return err
		}
		MergeHashedStats(rowsCount, hashedStats)
		rows := data.GetRowNum()
		size := data.GetMemorySize()
		totalRows += rows
		totalSize += size
		log.Info("reading file stat...", WrapLogFields(task, zap.Int("readRows", rows), zap.Int("readSize", size))...)
	}

	stat := &datapb.ImportFileStats{
		FileSize:        fileSize,
		TotalRows:       int64(totalRows),
		TotalMemorySize: int64(totalSize),
		HashedStats:     hashedStats,
	}
	p.manager.Update(task.GetTaskID(), UpdateFileStat(fileIdx, stat))
	return nil
}

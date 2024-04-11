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

package utils

import (
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	etcdKV "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/metastore/kv/querycoord"
	"github.com/milvus-io/milvus/internal/metastore/mocks"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	. "github.com/milvus-io/milvus/internal/querycoordv2/params"
	"github.com/milvus-io/milvus/internal/querycoordv2/session"
	"github.com/milvus-io/milvus/pkg/util/etcd"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

func TestSpawnReplicasWithRG(t *testing.T) {
	paramtable.Init()
	config := GenerateEtcdConfig()
	cli, _ := etcd.GetEtcdClient(
		config.UseEmbedEtcd.GetAsBool(),
		config.EtcdUseSSL.GetAsBool(),
		config.Endpoints.GetAsStrings(),
		config.EtcdTLSCert.GetValue(),
		config.EtcdTLSKey.GetValue(),
		config.EtcdTLSCACert.GetValue(),
		config.EtcdTLSMinVersion.GetValue())
	kv := etcdKV.NewEtcdKV(cli, config.MetaRootPath.GetValue())

	store := querycoord.NewCatalog(kv)
	nodeMgr := session.NewNodeManager()
	m := meta.NewMeta(RandomIncrementIDAllocator(), store, nodeMgr)
	m.ResourceManager.AddResourceGroup("rg1")
	m.ResourceManager.AddResourceGroup("rg2")
	m.ResourceManager.AddResourceGroup("rg3")
	for i := 1; i < 10; i++ {
		nodeMgr.Add(session.NewNodeInfo(session.ImmutableNodeInfo{
			NodeID:   int64(i),
			Address:  "localhost",
			Hostname: "localhost",
		}))
		if i%3 == 0 {
			m.ResourceManager.AssignNode("rg1", int64(i))
		}
		if i%3 == 1 {
			m.ResourceManager.AssignNode("rg2", int64(i))
		}
		if i%3 == 2 {
			m.ResourceManager.AssignNode("rg3", int64(i))
		}
	}

	type args struct {
		m              *meta.Meta
		collection     int64
		resourceGroups []string
		replicaNumber  int32
	}

	tests := []struct {
		name           string
		args           args
		wantReplicaNum int
		wantErr        bool
	}{
		{
			name:           "test 3 replica on 1 rg",
			args:           args{m, 1000, []string{"rg1"}, 3},
			wantReplicaNum: 3,
			wantErr:        false,
		},

		{
			name:           "test 3 replica on 2 rg",
			args:           args{m, 1001, []string{"rg1", "rg2"}, 3},
			wantReplicaNum: 0,
			wantErr:        true,
		},

		{
			name:           "test 3 replica on 3 rg",
			args:           args{m, 1002, []string{"rg1", "rg2", "rg3"}, 3},
			wantReplicaNum: 3,
			wantErr:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SpawnReplicasWithRG(tt.args.m, tt.args.collection, tt.args.resourceGroups, tt.args.replicaNumber)
			if (err != nil) != tt.wantErr {
				t.Errorf("SpawnReplicasWithRG() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(got) != tt.wantReplicaNum {
				t.Errorf("SpawnReplicasWithRG() = %v, want %d replicas", got, tt.args.replicaNumber)
			}
		})
	}
}

func TestAddNodesToCollectionsInRGFailed(t *testing.T) {
	paramtable.Init()

	store := mocks.NewQueryCoordCatalog(t)
	store.EXPECT().SaveCollection(mock.Anything).Return(nil)
	store.EXPECT().SaveReplica(mock.Anything).Return(nil).Times(4)
	store.EXPECT().SaveResourceGroup(mock.Anything).Return(nil)
	store.EXPECT().SaveResourceGroup(mock.Anything, mock.Anything).Return(nil)
	nodeMgr := session.NewNodeManager()
	m := meta.NewMeta(RandomIncrementIDAllocator(), store, nodeMgr)
	m.ResourceManager.AddResourceGroup("rg")
	m.CollectionManager.PutCollection(CreateTestCollection(1, 2))
	m.CollectionManager.PutCollection(CreateTestCollection(2, 2))
	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            1,
			CollectionID:  1,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            2,
			CollectionID:  1,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            3,
			CollectionID:  2,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            4,
			CollectionID:  2,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	storeErr := errors.New("store error")
	store.EXPECT().SaveReplica(mock.Anything).Return(storeErr)
	RecoverAllCollection(m)

	assert.Len(t, m.ReplicaManager.Get(1).GetNodes(), 0)
	assert.Len(t, m.ReplicaManager.Get(2).GetNodes(), 0)
	assert.Len(t, m.ReplicaManager.Get(3).GetNodes(), 0)
	assert.Len(t, m.ReplicaManager.Get(4).GetNodes(), 0)
}

func TestAddNodesToCollectionsInRG(t *testing.T) {
	paramtable.Init()

	store := mocks.NewQueryCoordCatalog(t)
	store.EXPECT().SaveCollection(mock.Anything).Return(nil)
	store.EXPECT().SaveReplica(mock.Anything).Return(nil)
	store.EXPECT().SaveReplica(mock.Anything, mock.Anything).Return(nil)
	store.EXPECT().SaveResourceGroup(mock.Anything).Return(nil)
	store.EXPECT().SaveResourceGroup(mock.Anything, mock.Anything).Return(nil)
	nodeMgr := session.NewNodeManager()
	m := meta.NewMeta(RandomIncrementIDAllocator(), store, nodeMgr)
	m.ResourceManager.AddResourceGroup("rg")
	m.CollectionManager.PutCollection(CreateTestCollection(1, 2))
	m.CollectionManager.PutCollection(CreateTestCollection(2, 2))
	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            1,
			CollectionID:  1,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            2,
			CollectionID:  1,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            3,
			CollectionID:  2,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))

	m.ReplicaManager.Put(meta.NewReplica(
		&querypb.Replica{
			ID:            4,
			CollectionID:  2,
			Nodes:         []int64{},
			ResourceGroup: "rg",
		},
		typeutil.NewUniqueSet(),
	))
	nodeMgr.Add(session.NewNodeInfo(session.ImmutableNodeInfo{
		NodeID:  1,
		Address: "localhost",
	}))
	_, err := m.ResourceManager.HandleNodeUp(1)
	assert.NoError(t, err)
	nodeMgr.Add(session.NewNodeInfo(session.ImmutableNodeInfo{
		NodeID:  2,
		Address: "localhost",
	}))
	_, err = m.ResourceManager.HandleNodeUp(2)
	assert.NoError(t, err)
	nodeMgr.Add(session.NewNodeInfo(session.ImmutableNodeInfo{
		NodeID:  3,
		Address: "localhost",
	}))
	_, err = m.ResourceManager.HandleNodeUp(3)
	assert.NoError(t, err)
	nodeMgr.Add(session.NewNodeInfo(session.ImmutableNodeInfo{
		NodeID:  4,
		Address: "localhost",
	}))
	_, err = m.ResourceManager.HandleNodeUp(4)
	assert.NoError(t, err)
	_, err = m.ResourceManager.TransferNode(meta.DefaultResourceGroupName, "rg", 4)
	assert.NoError(t, err)
	RecoverAllCollection(m)

	assert.Len(t, m.ReplicaManager.Get(1).GetNodes(), 2)
	assert.Len(t, m.ReplicaManager.Get(2).GetNodes(), 2)
	assert.Len(t, m.ReplicaManager.Get(3).GetNodes(), 2)
	assert.Len(t, m.ReplicaManager.Get(4).GetNodes(), 2)
}

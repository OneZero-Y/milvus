// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package proxy

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/mockey"
	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/mocks"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/dependency"
	"github.com/milvus-io/milvus/internal/util/function"
	"github.com/milvus-io/milvus/internal/util/reduce"
	"github.com/milvus-io/milvus/pkg/v2/common"
	"github.com/milvus-io/milvus/pkg/v2/proto/internalpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/planpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/querypb"
	"github.com/milvus-io/milvus/pkg/v2/util/funcutil"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/metric"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/testutils"
	"github.com/milvus-io/milvus/pkg/v2/util/timerecord"
	"github.com/milvus-io/milvus/pkg/v2/util/tsoutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

func TestSearchTask_PostExecute(t *testing.T) {
	var err error

	var (
		qc  = NewMixCoordMock()
		ctx = context.TODO()
	)

	require.NoError(t, err)
	mgr := newShardClientMgr()

	err = InitMetaCache(ctx, qc, mgr)
	require.NoError(t, err)

	getSearchTask := func(t *testing.T, collName string) *searchTask {
		task := &searchTask{
			ctx:            ctx,
			collectionName: collName,
			SearchRequest: &internalpb.SearchRequest{
				IsTopkReduce: true,
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collName,
				Nq:             1,
				SearchParams:   getBaseSearchParams(),
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		task.SetTs(tsoutil.ComposeTSByTime(time.Now(), 0))
		return task
	}
	t.Run("Test empty result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_collection_empty_result" + funcutil.GenRandomStr()
		createColl(t, collName, qc)
		qt := getSearchTask(t, collName)
		err = qt.PreExecute(ctx)
		assert.NoError(t, err)

		assert.NotNil(t, qt.resultBuf)
		qt.resultBuf.Insert(&internalpb.SearchResults{})
		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
		assert.Equal(t, qt.result.GetStatus().GetErrorCode(), commonpb.ErrorCode_Success)
		assert.Equal(t, qt.resultSizeInsufficient, true)
		assert.Equal(t, qt.isTopkReduce, false)
	})

	t.Run("test search iterator v2", func(t *testing.T) {
		const (
			kRows  = 10
			kToken = "test-token"
		)

		collName := "test_collection_search_iterator_v2" + funcutil.GenRandomStr()
		collSchema := createColl(t, collName, qc)

		createIteratorSearchTask := func(t *testing.T, metricType string, rows int) *searchTask {
			ids := make([]int64, rows)
			for i := range ids {
				ids[i] = int64(i)
			}
			resultIDs := &schemapb.IDs{
				IdField: &schemapb.IDs_IntId{
					IntId: &schemapb.LongArray{
						Data: ids,
					},
				},
			}
			scores := make([]float32, rows)
			// proxy needs to reverse the score for negatively related metrics
			for i := range scores {
				if metric.PositivelyRelated(metricType) {
					scores[i] = float32(len(scores) - i)
				} else {
					scores[i] = -float32(i + 1)
				}
			}
			resultData := &schemapb.SearchResultData{
				Ids:        resultIDs,
				Scores:     scores,
				NumQueries: 1,
			}

			qt := &searchTask{
				ctx: ctx,
				SearchRequest: &internalpb.SearchRequest{
					Base: &commonpb.MsgBase{
						MsgType:  commonpb.MsgType_Search,
						SourceID: paramtable.GetNodeID(),
					},
					Nq: 1,
				},
				schema: newSchemaInfo(collSchema),
				request: &milvuspb.SearchRequest{
					CollectionName: collName,
				},
				queryInfos: []*planpb.QueryInfo{{
					SearchIteratorV2Info: &planpb.SearchIteratorV2Info{
						Token:     kToken,
						BatchSize: 1,
					},
				}},
				result: &milvuspb.SearchResults{
					Results: resultData,
				},
				resultBuf:  typeutil.NewConcurrentSet[*internalpb.SearchResults](),
				tr:         timerecord.NewTimeRecorder("search"),
				isIterator: true,
			}
			bytes, err := proto.Marshal(resultData)
			assert.NoError(t, err)
			qt.resultBuf.Insert(&internalpb.SearchResults{
				MetricType: metricType,
				SlicedBlob: bytes,
			})
			return qt
		}

		t.Run("test search iterator v2", func(t *testing.T) {
			metrics := []string{metric.L2, metric.IP, metric.COSINE, metric.BM25}
			for _, metricType := range metrics {
				qt := createIteratorSearchTask(t, metricType, kRows)
				err = qt.PostExecute(ctx)
				assert.NoError(t, err)
				assert.Equal(t, kToken, qt.result.Results.SearchIteratorV2Results.Token)
				if metric.PositivelyRelated(metricType) {
					assert.Equal(t, float32(1), qt.result.Results.SearchIteratorV2Results.LastBound)
				} else {
					assert.Equal(t, float32(kRows), qt.result.Results.SearchIteratorV2Results.LastBound)
				}
			}
		})

		t.Run("test search iterator v2 with empty result", func(t *testing.T) {
			metrics := []string{metric.L2, metric.IP, metric.COSINE, metric.BM25}
			for _, metricType := range metrics {
				qt := createIteratorSearchTask(t, metricType, 0)
				err = qt.PostExecute(ctx)
				assert.NoError(t, err)
				assert.Equal(t, kToken, qt.result.Results.SearchIteratorV2Results.Token)
				if metric.PositivelyRelated(metricType) {
					assert.Equal(t, float32(math.MaxFloat32), qt.result.Results.SearchIteratorV2Results.LastBound)
				} else {
					assert.Equal(t, float32(-math.MaxFloat32), qt.result.Results.SearchIteratorV2Results.LastBound)
				}
			}
		})

		t.Run("test search iterator v2 with empty result and incoming last bound", func(t *testing.T) {
			metrics := []string{metric.L2, metric.IP, metric.COSINE, metric.BM25}
			kLastBound := float32(10)
			for _, metricType := range metrics {
				qt := createIteratorSearchTask(t, metricType, 0)
				qt.queryInfos[0].SearchIteratorV2Info.LastBound = &kLastBound
				err = qt.PostExecute(ctx)
				assert.NoError(t, err)
				assert.Equal(t, kToken, qt.result.Results.SearchIteratorV2Results.Token)
				assert.Equal(t, kLastBound, qt.result.Results.SearchIteratorV2Results.LastBound)
			}
		})
	})

	getSearchTaskWithRerank := func(t *testing.T, collName string, funcInput string) *searchTask {
		functionSchema := &schemapb.FunctionSchema{
			Name:            "test",
			Type:            schemapb.FunctionType_Rerank,
			InputFieldNames: []string{funcInput},
			Params: []*commonpb.KeyValuePair{
				{Key: "reranker", Value: "decay"},
				{Key: "origin", Value: "4"},
				{Key: "scale", Value: "4"},
				{Key: "offset", Value: "4"},
				{Key: "decay", Value: "0.5"},
				{Key: "function", Value: "gauss"},
			},
		}
		task := &searchTask{
			ctx:            ctx,
			collectionName: collName,
			SearchRequest: &internalpb.SearchRequest{
				IsTopkReduce: true,
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collName,
				Nq:             1,
				SearchParams:   getBaseSearchParams(),
				FunctionScore: &schemapb.FunctionScore{
					Functions: []*schemapb.FunctionSchema{functionSchema},
				},
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		return task
	}

	t.Run("Test empty result with rerank", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_collection_empty_result_with_rerank" + funcutil.GenRandomStr()
		createCollWithFields(t, collName, qc)
		qt := getSearchTaskWithRerank(t, collName, testFloatField)
		err = qt.PreExecute(ctx)
		assert.NoError(t, err)

		assert.NotNil(t, qt.resultBuf)
		qt.resultBuf.Insert(&internalpb.SearchResults{})
		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
		assert.Equal(t, qt.resultSizeInsufficient, true)
		assert.Equal(t, qt.isTopkReduce, false)
	})

	t.Run("Test search rerank", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_collection_empty_result_with_rerank" + funcutil.GenRandomStr()
		_, fieldNameId := createCollWithFields(t, collName, qc)
		qt := getSearchTaskWithRerank(t, collName, testFloatField)
		err = qt.PreExecute(ctx)
		assert.NoError(t, err)

		assert.NotNil(t, qt.resultBuf)
		qt.resultBuf.Insert(genTestSearchResultData(1, 10, schemapb.DataType_Float, testFloatField, fieldNameId[testFloatField], false))
		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
		assert.Equal(t, []int64{10}, qt.result.Results.Topks)
		assert.Equal(t, int64(10), qt.result.Results.TopK)
		assert.Equal(t, []int64{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}, qt.result.Results.Ids.GetIntId().Data)
	})

	getHybridSearchTaskWithRerank := func(t *testing.T, collName string, funcInput string, data [][]string) *searchTask {
		subReqs := []*milvuspb.SubSearchRequest{}
		for _, item := range data {
			placeholderValue := &commonpb.PlaceholderValue{
				Tag:    "$0",
				Type:   commonpb.PlaceholderType_VarChar,
				Values: lo.Map(item, func(str string, _ int) []byte { return []byte(str) }),
			}
			holder := &commonpb.PlaceholderGroup{
				Placeholders: []*commonpb.PlaceholderValue{placeholderValue},
			}
			holderByte, _ := proto.Marshal(holder)
			subReq := &milvuspb.SubSearchRequest{
				PlaceholderGroup: holderByte,
				SearchParams: []*commonpb.KeyValuePair{
					{Key: AnnsFieldKey, Value: testFloatVecField},
					{Key: TopKKey, Value: "10"},
				},
				Nq: int64(len(item)),
			}
			subReqs = append(subReqs, subReq)
		}
		functionSchema := &schemapb.FunctionSchema{
			Name:            "test",
			Type:            schemapb.FunctionType_Rerank,
			InputFieldNames: []string{funcInput},
			Params: []*commonpb.KeyValuePair{
				{Key: "reranker", Value: "decay"},
				{Key: "origin", Value: "4"},
				{Key: "scale", Value: "4"},
				{Key: "offset", Value: "4"},
				{Key: "decay", Value: "0.5"},
				{Key: "function", Value: "gauss"},
			},
		}
		task := &searchTask{
			ctx:            ctx,
			collectionName: collName,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:   commonpb.MsgType_Search,
					Timestamp: uint64(time.Now().UnixNano()),
				},
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collName,
				SubReqs:        subReqs,
				SearchParams: []*commonpb.KeyValuePair{
					{Key: LimitKey, Value: "10"},
				},
				FunctionScore: &schemapb.FunctionScore{
					Functions: []*schemapb.FunctionSchema{functionSchema},
				},
				OutputFields: []string{testInt32Field},
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		return task
	}

	t.Run("Test hybridsearch all empty result with rerank", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_collection_empty_result_with_rerank" + funcutil.GenRandomStr()
		createCollWithFields(t, collName, qc)
		qt := getHybridSearchTaskWithRerank(t, collName, testFloatField, [][]string{{"sentence"}, {"sentence"}})
		err = qt.PreExecute(ctx)
		assert.NoError(t, err)
		assert.NotNil(t, qt.resultBuf)
		qt.resultBuf.Insert(&internalpb.SearchResults{})
		qt.resultBuf.Insert(&internalpb.SearchResults{})
		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
	})

	t.Run("Test hybridsearch search rerank with empty", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_hybridsearch_rerank_with_empty" + funcutil.GenRandomStr()
		_, fieldNameId := createCollWithFields(t, collName, qc)
		qt := getHybridSearchTaskWithRerank(t, collName, testFloatField, [][]string{{"sentence", "sentence"}, {"sentence", "sentence"}})
		err = qt.PreExecute(ctx)
		assert.Equal(t, qt.Nq, int64(2))
		assert.NoError(t, err)
		assert.NotNil(t, qt.resultBuf)
		// All data are from the same subsearch
		qt.resultBuf.Insert(genTestSearchResultData(2, 10, schemapb.DataType_Int64, testInt64Field, fieldNameId[testInt64Field], true))
		qt.resultBuf.Insert(genTestSearchResultData(2, 10, schemapb.DataType_Int64, testInt64Field, fieldNameId[testInt64Field], true))
		// rerank inputs
		f1 := testutils.GenerateScalarFieldData(schemapb.DataType_Float, testFloatField, 20)
		f1.FieldId = fieldNameId[testFloatField]
		// search output field
		f2 := testutils.GenerateScalarFieldData(schemapb.DataType_Int32, testInt32Field, 20)
		f2.FieldId = fieldNameId[testInt32Field]
		// pk
		f3 := testutils.GenerateScalarFieldData(schemapb.DataType_Int64, testInt64Field, 20)
		f3.FieldId = fieldNameId[testInt64Field]

		mocker := mockey.Mock((*requeryOperator).requery).Return(&milvuspb.QueryResults{
			FieldsData: []*schemapb.FieldData{f1, f2, f3},
		}, nil).Build()
		defer mocker.UnPatch()

		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
		assert.Equal(t, []int64{10, 10}, qt.result.Results.Topks)
		assert.Equal(t, int64(10), qt.result.Results.TopK)
		assert.Equal(t, int64(2), qt.result.Results.NumQueries)
		assert.Equal(t, []int64{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}, qt.result.Results.Ids.GetIntId().Data)
		assert.Equal(t, testInt32Field, qt.result.Results.FieldsData[0].FieldName)
	})

	t.Run("Test hybridsearch search rerank ", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_hybridsearch_result_with_rerank" + funcutil.GenRandomStr()
		_, fieldNameId := createCollWithFields(t, collName, qc)
		qt := getHybridSearchTaskWithRerank(t, collName, testFloatField, [][]string{{"sentence", "sentence"}, {"sentence", "sentence"}})
		err = qt.PreExecute(ctx)
		assert.Equal(t, qt.Nq, int64(2))
		assert.NoError(t, err)
		assert.NotNil(t, qt.resultBuf)
		data1 := genTestSearchResultData(2, 10, schemapb.DataType_Int64, testInt64Field, fieldNameId[testInt64Field], true)
		data1.SubResults[0].ReqIndex = 0
		data2 := genTestSearchResultData(2, 10, schemapb.DataType_Int64, testInt64Field, fieldNameId[testInt64Field], true)
		data1.SubResults[0].ReqIndex = 2
		qt.resultBuf.Insert(data2)

		// rerank inputs
		f1 := testutils.GenerateScalarFieldData(schemapb.DataType_Float, testFloatField, 20)
		f1.FieldId = fieldNameId[testFloatField]
		// search output field
		f2 := testutils.GenerateScalarFieldData(schemapb.DataType_Int32, testInt32Field, 20)
		f2.FieldId = fieldNameId[testInt32Field]
		// pk
		f3 := testutils.GenerateScalarFieldData(schemapb.DataType_Int64, testInt64Field, 20)
		f3.FieldId = fieldNameId[testInt64Field]

		mocker := mockey.Mock((*requeryOperator).requery).Return(&milvuspb.QueryResults{
			FieldsData: []*schemapb.FieldData{f1, f2, f3},
		}, nil).Build()
		defer mocker.UnPatch()

		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
		assert.Equal(t, []int64{10, 10}, qt.result.Results.Topks)
		assert.Equal(t, int64(10), qt.result.Results.TopK)
		assert.Equal(t, int64(2), qt.result.Results.NumQueries)
		assert.Equal(t, []int64{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}, qt.result.Results.Ids.GetIntId().Data)
		assert.Equal(t, testInt32Field, qt.result.Results.FieldsData[0].FieldName)
	})

	// rrf/weigted rank
	t.Run("Test rank function", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		collName := "test_rank_function" + funcutil.GenRandomStr()
		_, fieldNameId := createCollWithFields(t, collName, qc)
		qt := getHybridSearchTaskWithRerank(t, collName, testFloatField, [][]string{{"sentence", "sentence"}, {"sentence", "sentence"}})
		qt.request.FunctionScore = nil
		qt.request.SearchParams = []*commonpb.KeyValuePair{{Key: "limit", Value: "10"}}
		qt.request.OutputFields = []string{"*"}
		err = qt.PreExecute(ctx)
		assert.NoError(t, err)
		data1 := genTestSearchResultData(2, 10, schemapb.DataType_Int64, testInt64Field, fieldNameId[testInt64Field], true)
		data1.SubResults[0].ReqIndex = 0
		data2 := genTestSearchResultData(2, 10, schemapb.DataType_Int64, testInt64Field, fieldNameId[testInt64Field], true)
		data1.SubResults[0].ReqIndex = 2
		qt.resultBuf.Insert(data2)

		f1 := testutils.GenerateScalarFieldData(schemapb.DataType_Int32, testInt32Field, 20)
		f1.FieldId = fieldNameId[testInt32Field]
		f2 := testutils.GenerateVectorFieldData(schemapb.DataType_FloatVector, testFloatVecField, 20, testVecDim)
		f2.FieldId = fieldNameId[testFloatVecField]
		f3 := testutils.GenerateScalarFieldData(schemapb.DataType_Int64, testInt64Field, 20)
		f3.FieldId = fieldNameId[testInt64Field]
		f4 := testutils.GenerateScalarFieldData(schemapb.DataType_Float, testFloatField, 20)
		f4.FieldId = fieldNameId[testFloatField]
		mocker := mockey.Mock((*requeryOperator).requery).Return(&milvuspb.QueryResults{
			FieldsData: []*schemapb.FieldData{f1, f2, f3, f4},
		}, nil).Build()
		defer mocker.UnPatch()

		err := qt.PostExecute(context.TODO())
		assert.NoError(t, err)
		assert.Equal(t, []int64{10, 10}, qt.result.Results.Topks)
		assert.Equal(t, int64(10), qt.result.Results.TopK)
		assert.Equal(t, int64(2), qt.result.Results.NumQueries)
		assert.Equal(t, []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}, qt.result.Results.Ids.GetIntId().Data)
		for _, field := range qt.result.Results.FieldsData {
			switch field.FieldName {
			case testInt32Field:
				assert.True(t, len(field.GetScalars().GetIntData().Data) != 0)
			case testBoolField:
				assert.True(t, len(field.GetScalars().GetBoolData().Data) != 0)
			case testFloatField:
				assert.True(t, len(field.GetScalars().GetFloatData().Data) != 0)
			case testFloatVecField:
				assert.True(t, len(field.GetVectors().GetFloatVector().Data) != 0)
			case testInt64Field:
				assert.True(t, len(field.GetScalars().GetLongData().Data) != 0)
			}
		}
	})
}

func createCollWithFields(t *testing.T, collName string, rc types.MixCoordClient) (*schemapb.CollectionSchema, map[string]int64) {
	fieldName2Types := map[string]schemapb.DataType{
		testInt64Field:    schemapb.DataType_Int64,
		testFloatField:    schemapb.DataType_Float,
		testFloatVecField: schemapb.DataType_FloatVector,
		testInt32Field:    schemapb.DataType_Int32,
		testBoolField:     schemapb.DataType_Bool,
	}
	schema := constructCollectionSchemaByDataType(collName, fieldName2Types, testInt64Field, true)
	marshaledSchema, err := proto.Marshal(schema)
	assert.NoError(t, err)
	ctx := context.TODO()

	createColT := &createCollectionTask{
		Condition: NewTaskCondition(ctx),
		CreateCollectionRequest: &milvuspb.CreateCollectionRequest{
			CollectionName: collName,
			Schema:         marshaledSchema,
			ShardsNum:      1,
		},
		ctx:      ctx,
		mixCoord: rc,
	}

	require.NoError(t, createColT.OnEnqueue())
	require.NoError(t, createColT.PreExecute(ctx))
	require.NoError(t, createColT.Execute(ctx))
	require.NoError(t, createColT.PostExecute(ctx))

	_, err = globalMetaCache.GetCollectionID(ctx, GetCurDBNameFromContextOrDefault(ctx), collName)
	assert.NoError(t, err)

	fieldNameId := make(map[string]int64)
	for _, field := range schema.Fields {
		fieldNameId[field.Name] = field.FieldID
	}
	return schema, fieldNameId
}

func createColl(t *testing.T, name string, rc types.MixCoordClient) *schemapb.CollectionSchema {
	schema := constructCollectionSchema(testInt64Field, testFloatVecField, testVecDim, name)
	marshaledSchema, err := proto.Marshal(schema)
	require.NoError(t, err)
	ctx := context.TODO()

	createColT := &createCollectionTask{
		Condition: NewTaskCondition(context.TODO()),
		CreateCollectionRequest: &milvuspb.CreateCollectionRequest{
			CollectionName: name,
			Schema:         marshaledSchema,
			ShardsNum:      common.DefaultShardsNum,
		},
		ctx:      ctx,
		mixCoord: rc,
	}

	require.NoError(t, createColT.OnEnqueue())
	require.NoError(t, createColT.PreExecute(ctx))
	require.NoError(t, createColT.Execute(ctx))
	require.NoError(t, createColT.PostExecute(ctx))

	return schema
}

func getBaseSearchParams() []*commonpb.KeyValuePair {
	return []*commonpb.KeyValuePair{
		{
			Key:   AnnsFieldKey,
			Value: testFloatVecField,
		},
		{
			Key:   TopKKey,
			Value: "10",
		},
		{
			Key:   "analyzer_name",
			Value: "test_analyzer",
		}, // invalid analyzer
	}
}

func getValidSearchParams() []*commonpb.KeyValuePair {
	return []*commonpb.KeyValuePair{
		{
			Key:   AnnsFieldKey,
			Value: testFloatVecField,
		},
		{
			Key:   TopKKey,
			Value: "10",
		},
		{
			Key:   common.MetricTypeKey,
			Value: metric.L2,
		},
		{
			Key:   ParamsKey,
			Value: `{"nprobe": 10}`,
		},
		{
			Key:   RoundDecimalKey,
			Value: "-1",
		},
		{
			Key:   IgnoreGrowingKey,
			Value: "false",
		},
	}
}

func resetSearchParamsValue(kvs []*commonpb.KeyValuePair, keyName string, newVal string) {
	for _, kv := range kvs {
		if kv.GetKey() == keyName {
			kv.Value = newVal
		}
	}
}

func getInvalidSearchParams(invalidName string) []*commonpb.KeyValuePair {
	kvs := getValidSearchParams()
	for _, kv := range kvs {
		if kv.GetKey() == invalidName {
			kv.Value = "invalid"
		}
	}
	return kvs
}

func TestSearchTask_PreExecute(t *testing.T) {
	var err error

	var (
		qc  = NewMixCoordMock()
		ctx = context.TODO()
	)
	require.NoError(t, err)
	mgr := newShardClientMgr()
	err = InitMetaCache(ctx, qc, mgr)
	require.NoError(t, err)

	getSearchTask := func(t *testing.T, collName string) *searchTask {
		task := &searchTask{
			ctx:            ctx,
			collectionName: collName,
			SearchRequest:  &internalpb.SearchRequest{},
			request: &milvuspb.SearchRequest{
				CollectionName: collName,
				Nq:             1,
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		task.SetTs(tsoutil.ComposeTSByTime(time.Now(), 0))
		return task
	}

	getSearchTaskWithNq := func(t *testing.T, collName string, nq int64) *searchTask {
		task := &searchTask{
			ctx:            ctx,
			collectionName: collName,
			SearchRequest:  &internalpb.SearchRequest{},
			request: &milvuspb.SearchRequest{
				CollectionName: collName,
				Nq:             nq,
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		task.SetTs(tsoutil.ComposeTSByTime(time.Now(), 0))
		return task
	}

	getSearchTaskWithRerank := func(t *testing.T, collName string, funcInput string) *searchTask {
		functionSchema := &schemapb.FunctionSchema{
			Name:            "test",
			Type:            schemapb.FunctionType_Rerank,
			InputFieldNames: []string{funcInput},
			Params: []*commonpb.KeyValuePair{
				{Key: "reranker", Value: "decay"},
				{Key: "origin", Value: "4"},
				{Key: "scale", Value: "4"},
				{Key: "offset", Value: "4"},
				{Key: "decay", Value: "0.5"},
				{Key: "function", Value: "gauss"},
			},
		}
		task := &searchTask{
			ctx:            ctx,
			collectionName: collName,
			SearchRequest:  &internalpb.SearchRequest{},
			request: &milvuspb.SearchRequest{
				CollectionName: collName,
				Nq:             1,
				SubReqs:        []*milvuspb.SubSearchRequest{},
				FunctionScore: &schemapb.FunctionScore{
					Functions: []*schemapb.FunctionSchema{functionSchema},
				},
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		task.SetTs(tsoutil.ComposeTSByTime(time.Now(), 0))
		return task
	}

	t.Run("bad nq 0", func(t *testing.T) {
		collName := "test_bad_nq0_error" + funcutil.GenRandomStr()
		createColl(t, collName, qc)
		// Nq must be in range [1, 16384].
		task := getSearchTaskWithNq(t, collName, 0)
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	})

	t.Run("bad nq 16385", func(t *testing.T) {
		collName := "test_bad_nq16385_error" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		// Nq must be in range [1, 16384].
		task := getSearchTaskWithNq(t, collName, 16384+1)
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	})

	t.Run("reject large num of result entries", func(t *testing.T) {
		collName := "test_large_num_of_result_entries" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		task := getSearchTask(t, collName)
		task.SearchRequest.Nq = 1000
		task.SearchRequest.Topk = 1001
		err = task.PreExecute(ctx)
		assert.Error(t, err)

		task.SearchRequest.Nq = 100
		task.SearchRequest.Topk = 100
		task.SearchRequest.GroupSize = 200
		err = task.PreExecute(ctx)
		assert.Error(t, err)

		task.SearchRequest.IsAdvanced = true
		task.SearchRequest.SubReqs = []*internalpb.SubSearchRequest{
			{
				Topk:      100,
				Nq:        100,
				GroupSize: 200,
			},
		}
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	})

	t.Run("collection not exist", func(t *testing.T) {
		collName := "test_collection_not_exist" + funcutil.GenRandomStr()
		task := getSearchTask(t, collName)
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	})

	t.Run("invalid IgnoreGrowing param", func(t *testing.T) {
		collName := "test_invalid_param" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		task := getSearchTask(t, collName)
		task.request.SearchParams = getInvalidSearchParams(IgnoreGrowingKey)
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	})

	t.Run("search with timeout", func(t *testing.T) {
		collName := "search_with_timeout" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		task := getSearchTask(t, collName)
		task.request.SearchParams = getValidSearchParams()
		task.request.DslType = commonpb.DslType_BoolExprV1

		ctxTimeout, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, task.TimeoutTimestamp)

		task.ctx = ctxTimeout
		assert.NoError(t, task.PreExecute(ctx))
		assert.Greater(t, task.TimeoutTimestamp, typeutil.ZeroTimestamp)

		{
			task.mustUsePartitionKey = true
			err = task.PreExecute(ctx)
			assert.Error(t, err)
			assert.ErrorIs(t, err, merr.ErrParameterInvalid)
			task.mustUsePartitionKey = false
		}

		// field not exist
		task.ctx = context.TODO()
		task.request.OutputFields = []string{testInt64Field + funcutil.GenRandomStr()}
		assert.Error(t, task.PreExecute(ctx))

		// contain vector field
		task.request.OutputFields = []string{testFloatVecField}
		assert.NoError(t, task.PreExecute(ctx))
	})

	t.Run("search consistent iterator pre_ts", func(t *testing.T) {
		collName := "search_with_timeout" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		st := getSearchTask(t, collName)
		st.request.SearchParams = getValidSearchParams()
		st.request.SearchParams = append(st.request.SearchParams, &commonpb.KeyValuePair{
			Key:   IteratorField,
			Value: "True",
		})
		st.request.GuaranteeTimestamp = 1000
		st.request.DslType = commonpb.DslType_BoolExprV1

		ctxTimeout, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, st.TimeoutTimestamp)

		st.ctx = ctxTimeout
		assert.NoError(t, st.PreExecute(ctx))
		assert.True(t, st.isIterator)
		assert.True(t, st.GetMvccTimestamp() > 0)
		assert.Equal(t, uint64(1000), st.GetGuaranteeTimestamp())
	})

	t.Run("search consistent iterator post_ts", func(t *testing.T) {
		collName := "search_with_timeout" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		st := getSearchTask(t, collName)
		st.request.SearchParams = getValidSearchParams()
		st.request.SearchParams = append(st.request.SearchParams, &commonpb.KeyValuePair{
			Key:   IteratorField,
			Value: "True",
		})
		st.request.DslType = commonpb.DslType_BoolExprV1

		_, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, st.TimeoutTimestamp)
		enqueueTs := tsoutil.ComposeTSByTime(time.Now(), 0)
		st.SetTs(enqueueTs)
		assert.NoError(t, st.PreExecute(ctx))
		assert.True(t, st.isIterator)
		assert.True(t, st.GetMvccTimestamp() == 0)
		st.resultBuf.Insert(&internalpb.SearchResults{})
		st.PostExecute(context.TODO())
		assert.Equal(t, st.result.GetSessionTs(), enqueueTs)
	})

	t.Run("search inconsistent collection_id", func(t *testing.T) {
		collName := "search_inconsistent_collection" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		st := getSearchTask(t, collName)
		st.request.SearchParams = getValidSearchParams()
		st.request.SearchParams = append(st.request.SearchParams, &commonpb.KeyValuePair{
			Key:   IteratorField,
			Value: "True",
		})
		st.request.SearchParams = append(st.request.SearchParams, &commonpb.KeyValuePair{
			Key:   CollectionID,
			Value: "8080",
		})
		st.request.DslType = commonpb.DslType_BoolExprV1

		_, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, st.TimeoutTimestamp)
		enqueueTs := tsoutil.ComposeTSByTime(time.Now(), 0)
		st.SetTs(enqueueTs)
		assert.Error(t, st.PreExecute(ctx))
	})

	t.Run("search_with_schema_updated", func(t *testing.T) {
		collName := "collection_updated" + funcutil.GenRandomStr()
		createColl(t, collName, qc)

		st := getSearchTask(t, collName)
		st.request.SearchParams = getValidSearchParams()
		st.request.DslType = commonpb.DslType_BoolExprV1
		st.request.UseDefaultConsistency = false
		st.request.ConsistencyLevel = commonpb.ConsistencyLevel_Eventually

		collInfo, err := globalMetaCache.GetCollectionInfo(ctx, "", collName, 0)
		assert.NoError(t, err)

		assert.NoError(t, st.PreExecute(ctx))
		assert.Equal(t, collInfo.updateTimestamp, st.SearchRequest.GuaranteeTimestamp)
	})
	t.Run("search with rerank", func(t *testing.T) {
		collName := "search_with_rerank" + funcutil.GenRandomStr()
		createCollWithFields(t, collName, qc)
		st := getSearchTaskWithRerank(t, collName, testFloatField)
		st.request.SearchParams = getValidSearchParams()
		st.request.DslType = commonpb.DslType_BoolExprV1

		_, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, st.TimeoutTimestamp)
		enqueueTs := tsoutil.ComposeTSByTime(time.Now(), 0)
		st.SetTs(enqueueTs)
		assert.NoError(t, st.PreExecute(ctx))
		assert.NotNil(t, st.functionScore)
		assert.Equal(t, false, st.SearchRequest.GetIsAdvanced())
	})

	t.Run("advance search with rerank", func(t *testing.T) {
		collName := "search_with_rerank" + funcutil.GenRandomStr()
		createCollWithFields(t, collName, qc)
		st := getSearchTaskWithRerank(t, collName, testFloatField)
		st.request.SearchParams = getValidSearchParams()
		st.request.SearchParams = append(st.request.SearchParams, &commonpb.KeyValuePair{
			Key:   LimitKey,
			Value: "10",
		})
		st.request.DslType = commonpb.DslType_BoolExprV1
		st.request.SubReqs = append(st.request.SubReqs, &milvuspb.SubSearchRequest{Nq: 1})
		st.request.SubReqs = append(st.request.SubReqs, &milvuspb.SubSearchRequest{Nq: 1})
		_, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, st.TimeoutTimestamp)
		enqueueTs := tsoutil.ComposeTSByTime(time.Now(), 0)
		st.SetTs(enqueueTs)
		assert.NoError(t, st.PreExecute(ctx))
		assert.NotNil(t, st.functionScore)
		assert.Equal(t, true, st.SearchRequest.GetIsAdvanced())
	})

	t.Run("search with rerank grouping", func(t *testing.T) {
		collName := "search_with_rerank" + funcutil.GenRandomStr()
		createCollWithFields(t, collName, qc)
		st := getSearchTaskWithRerank(t, collName, testFloatField)
		st.request.SearchParams = getValidSearchParams()
		st.request.DslType = commonpb.DslType_BoolExprV1

		st.request.SearchParams = append(st.request.SearchParams, &commonpb.KeyValuePair{
			Key:   GroupByFieldKey,
			Value: testInt64Field,
		})

		_, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Equal(t, typeutil.ZeroTimestamp, st.TimeoutTimestamp)
		enqueueTs := tsoutil.ComposeTSByTime(time.Now(), 0)
		st.SetTs(enqueueTs)
		assert.NoError(t, st.PreExecute(ctx))
	})
}

func TestSearchTask_WithFunctions(t *testing.T) {
	paramtable.Init()
	paramtable.Get().CredentialCfg.Credential.GetFunc = func() map[string]string {
		return map[string]string{
			"mock.apikey": "mock",
		}
	}
	ts := function.CreateOpenAIEmbeddingServer()
	defer ts.Close()
	paramtable.Get().FunctionCfg.TextEmbeddingProviders.GetFunc = func() map[string]string {
		return map[string]string{
			"openai.url": ts.URL,
		}
	}
	collectionName := "TestSearchTask_function"
	schema := &schemapb.CollectionSchema{
		Name:        collectionName,
		Description: "TestSearchTask_function",
		AutoID:      true,
		Fields: []*schemapb.FieldSchema{
			{FieldID: 100, Name: "id", DataType: schemapb.DataType_Int64, IsPrimaryKey: true, AutoID: true},
			{
				FieldID: 101, Name: "text", DataType: schemapb.DataType_VarChar,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: "max_length", Value: "200"},
				},
			},
			{
				FieldID: 102, Name: "vector1", DataType: schemapb.DataType_FloatVector,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: "dim", Value: "4"},
				},
			},
			{
				FieldID: 103, Name: "vector2", DataType: schemapb.DataType_FloatVector,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: "dim", Value: "4"},
				},
			},
			{
				FieldID: 104, Name: "ts", DataType: schemapb.DataType_Int64,
			},
		},
		Functions: []*schemapb.FunctionSchema{
			{
				Name:             "func1",
				Type:             schemapb.FunctionType_TextEmbedding,
				InputFieldIds:    []int64{101},
				InputFieldNames:  []string{"text"},
				OutputFieldIds:   []int64{102},
				OutputFieldNames: []string{"vector1"},
				Params: []*commonpb.KeyValuePair{
					{Key: "provider", Value: "openai"},
					{Key: "model_name", Value: "text-embedding-ada-002"},
					{Key: "credential", Value: "mock"},
					{Key: "dim", Value: "4"},
				},
			},
			{
				Name:             "func2",
				Type:             schemapb.FunctionType_TextEmbedding,
				InputFieldIds:    []int64{101},
				InputFieldNames:  []string{"text"},
				OutputFieldIds:   []int64{103},
				OutputFieldNames: []string{"vector2"},
				Params: []*commonpb.KeyValuePair{
					{Key: "provider", Value: "openai"},
					{Key: "model_name", Value: "text-embedding-ada-002"},
					{Key: "credential", Value: "mock"},
					{Key: "dim", Value: "4"},
				},
			},
		},
	}

	var err error
	var (
		qc  = NewMixCoordMock()
		ctx = context.TODO()
	)

	require.NoError(t, err)
	mgr := newShardClientMgr()
	err = InitMetaCache(ctx, qc, mgr)
	require.NoError(t, err)

	getSearchTask := func(t *testing.T, collName string, data []string, withRerank bool) *searchTask {
		placeholderValue := &commonpb.PlaceholderValue{
			Tag:    "$0",
			Type:   commonpb.PlaceholderType_VarChar,
			Values: lo.Map(data, func(str string, _ int) []byte { return []byte(str) }),
		}
		holder := &commonpb.PlaceholderGroup{
			Placeholders: []*commonpb.PlaceholderValue{placeholderValue},
		}
		holderByte, _ := proto.Marshal(holder)

		functionSchema := &schemapb.FunctionSchema{
			Name:            "test",
			Type:            schemapb.FunctionType_Rerank,
			InputFieldNames: []string{"ts"},
			Params: []*commonpb.KeyValuePair{
				{Key: "reranker", Value: "decay"},
				{Key: "origin", Value: "4"},
				{Key: "scale", Value: "4"},
				{Key: "offset", Value: "4"},
				{Key: "decay", Value: "0.5"},
				{Key: "function", Value: "gauss"},
			},
		}

		task := &searchTask{
			ctx:            ctx,
			collectionName: collectionName,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:   commonpb.MsgType_Search,
					Timestamp: uint64(time.Now().UnixNano()),
				},
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collectionName,
				Nq:             int64(len(data)),
				SearchParams: []*commonpb.KeyValuePair{
					{Key: AnnsFieldKey, Value: "vector1"},
					{Key: TopKKey, Value: "10"},
				},
				PlaceholderGroup: holderByte,
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		if withRerank {
			task.request.FunctionScore = &schemapb.FunctionScore{
				Functions: []*schemapb.FunctionSchema{functionSchema},
			}
		}
		require.NoError(t, task.OnEnqueue())
		return task
	}

	collectionID := UniqueID(1000)
	cache := NewMockCache(t)
	info := newSchemaInfo(schema)
	cache.EXPECT().GetCollectionID(mock.Anything, mock.Anything, mock.Anything).Return(collectionID, nil).Maybe()
	cache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(info, nil).Maybe()
	cache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).Return(map[string]int64{"_default": UniqueID(1)}, nil).Maybe()
	cache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(&collectionInfo{}, nil).Maybe()
	cache.EXPECT().GetShard(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]nodeInfo{}, nil).Maybe()
	cache.EXPECT().DeprecateShardCache(mock.Anything, mock.Anything).Return().Maybe()
	globalMetaCache = cache

	{
		task := getSearchTask(t, collectionName, []string{"sentence"}, false)
		err = task.PreExecute(ctx)
		assert.NoError(t, err)
		pb := &commonpb.PlaceholderGroup{}
		proto.Unmarshal(task.SearchRequest.PlaceholderGroup, pb)
		assert.Equal(t, len(pb.Placeholders), 1)
		assert.Equal(t, len(pb.Placeholders[0].Values), 1)
		assert.Equal(t, pb.Placeholders[0].Type, commonpb.PlaceholderType_FloatVector)
	}

	{
		task := getSearchTask(t, collectionName, []string{"sentence 1", "sentence 2"}, false)
		err = task.PreExecute(ctx)
		assert.NoError(t, err)
		pb := &commonpb.PlaceholderGroup{}
		proto.Unmarshal(task.SearchRequest.PlaceholderGroup, pb)
		assert.Equal(t, len(pb.Placeholders), 1)
		assert.Equal(t, len(pb.Placeholders[0].Values), 2)
		assert.Equal(t, pb.Placeholders[0].Type, commonpb.PlaceholderType_FloatVector)
	}

	// process failed
	{
		task := getSearchTask(t, collectionName, []string{"sentence"}, false)
		task.request.Nq = 10000
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	}

	getHybridSearchTask := func(t *testing.T, collName string, data [][]string) *searchTask {
		subReqs := []*milvuspb.SubSearchRequest{}
		for _, item := range data {
			placeholderValue := &commonpb.PlaceholderValue{
				Tag:    "$0",
				Type:   commonpb.PlaceholderType_VarChar,
				Values: lo.Map(item, func(str string, _ int) []byte { return []byte(str) }),
			}
			holder := &commonpb.PlaceholderGroup{
				Placeholders: []*commonpb.PlaceholderValue{placeholderValue},
			}
			holderByte, _ := proto.Marshal(holder)
			subReq := &milvuspb.SubSearchRequest{
				PlaceholderGroup: holderByte,
				SearchParams: []*commonpb.KeyValuePair{
					{Key: AnnsFieldKey, Value: "vector1"},
					{Key: TopKKey, Value: "10"},
				},
				Nq: int64(len(item)),
			}
			subReqs = append(subReqs, subReq)
		}
		task := &searchTask{
			ctx:            ctx,
			collectionName: collectionName,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:   commonpb.MsgType_Search,
					Timestamp: uint64(time.Now().UnixNano()),
				},
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collectionName,
				SubReqs:        subReqs,
				SearchParams: []*commonpb.KeyValuePair{
					{Key: LimitKey, Value: "10"},
				},
			},
			mixCoord: qc,
			tr:       timerecord.NewTimeRecorder("test-search"),
		}
		require.NoError(t, task.OnEnqueue())
		return task
	}

	{
		task := getHybridSearchTask(t, collectionName, [][]string{
			{"sentence1"},
			{"sentence2"},
		})
		err = task.PreExecute(ctx)
		assert.NoError(t, err)
		assert.Equal(t, len(task.SearchRequest.SubReqs), 2)
		for _, sub := range task.SearchRequest.SubReqs {
			pb := &commonpb.PlaceholderGroup{}
			proto.Unmarshal(sub.PlaceholderGroup, pb)
			assert.Equal(t, len(pb.Placeholders), 1)
			assert.Equal(t, len(pb.Placeholders[0].Values), 1)
			assert.Equal(t, pb.Placeholders[0].Type, commonpb.PlaceholderType_FloatVector)
		}
	}

	{
		task := getHybridSearchTask(t, collectionName, [][]string{
			{"sentence1", "sentence1"},
			{"sentence2", "sentence2"},
			{"sentence3", "sentence3"},
		})
		err = task.PreExecute(ctx)
		assert.NoError(t, err)
		assert.Equal(t, len(task.SearchRequest.SubReqs), 3)
		for _, sub := range task.SearchRequest.SubReqs {
			pb := &commonpb.PlaceholderGroup{}
			proto.Unmarshal(sub.PlaceholderGroup, pb)
			assert.Equal(t, len(pb.Placeholders), 1)
			assert.Equal(t, len(pb.Placeholders[0].Values), 2)
			assert.Equal(t, pb.Placeholders[0].Type, commonpb.PlaceholderType_FloatVector)
		}
	}
	// process failed
	{
		task := getHybridSearchTask(t, collectionName, [][]string{
			{"sentence1", "sentence1"},
			{"sentence2", "sentence2"},
			{"sentence3", "sentence3"},
		})
		task.request.SubReqs[0].Nq = 10000
		err = task.PreExecute(ctx)
		assert.Error(t, err)
	}
}

func getMixCoord() *mocks.MixCoord {
	mixc := &mocks.MixCoord{}
	mixc.EXPECT().Start().Return(nil)
	mixc.EXPECT().Stop().Return(nil)
	return mixc
}

func getMixCoordClient() *mocks.MockMixCoordClient {
	mixc := &mocks.MockMixCoordClient{}
	mixc.EXPECT().Close().Return(nil)
	return mixc
}

func getQueryNode() *mocks.MockQueryNode {
	qn := &mocks.MockQueryNode{}

	return qn
}

func getQueryNodeClient() *mocks.MockQueryNodeClient {
	qn := &mocks.MockQueryNodeClient{}

	return qn
}

func TestSearchTaskV2_Execute(t *testing.T) {
	var (
		err error

		qc  = NewMixCoordMock()
		ctx = context.TODO()

		collectionName = t.Name() + funcutil.GenRandomStr()
	)

	mgr := newShardClientMgr()
	err = InitMetaCache(ctx, qc, mgr)
	require.NoError(t, err)

	defer qc.Close()

	task := &searchTask{
		ctx: ctx,
		SearchRequest: &internalpb.SearchRequest{
			Base: &commonpb.MsgBase{
				MsgType:   commonpb.MsgType_Search,
				Timestamp: uint64(time.Now().UnixNano()),
			},
		},
		request: &milvuspb.SearchRequest{
			CollectionName: collectionName,
		},
		result: &milvuspb.SearchResults{
			Status: &commonpb.Status{},
		},
		mixCoord: qc,
		tr:       timerecord.NewTimeRecorder("search"),
	}
	require.NoError(t, task.OnEnqueue())
	createColl(t, collectionName, qc)
}

func genSearchResultData(nq int64, topk int64, ids []int64, scores []float32) *schemapb.SearchResultData {
	return &schemapb.SearchResultData{
		NumQueries: nq,
		TopK:       topk,
		FieldsData: nil,
		Scores:     scores,
		Ids: &schemapb.IDs{
			IdField: &schemapb.IDs_IntId{
				IntId: &schemapb.LongArray{
					Data: ids,
				},
			},
		},
		Topks: make([]int64, nq),
	}
}

func TestSearchTask_Ts(t *testing.T) {
	task := &searchTask{
		SearchRequest: &internalpb.SearchRequest{},

		tr: timerecord.NewTimeRecorder("test-search"),
	}
	require.NoError(t, task.OnEnqueue())

	ts := Timestamp(time.Now().Nanosecond())
	task.SetTs(ts)
	assert.Equal(t, ts, task.BeginTs())
	assert.Equal(t, ts, task.EndTs())
}

func TestSearchTaskWithInvalidRoundDecimal(t *testing.T) {
	// var err error
	//
	// Params.ProxyCfg.SearchResultChannelNames = []string{funcutil.GenRandomStr()}
	//
	// rc := NewRootCoordMock()
	// rc.Start()
	// defer rc.Stop()
	//
	// ctx := context.Background()
	//
	// err = InitMetaCache(ctx, rc)
	// assert.NoError(t, err)
	//
	// shardsNum := int32(2)
	// prefix := "TestSearchTaskV2_all"
	// collectionName := prefix + funcutil.GenRandomStr()
	//
	// dim := 128
	// expr := fmt.Sprintf("%s > 0", testInt64Field)
	// nq := 10
	// topk := 10
	// roundDecimal := 7
	// nprobe := 10
	//
	// fieldName2Types := map[string]schemapb.DataType{
	//     testBoolField:     schemapb.DataType_Bool,
	//     testInt32Field:    schemapb.DataType_Int32,
	//     testInt64Field:    schemapb.DataType_Int64,
	//     testFloatField:    schemapb.DataType_Float,
	//     testDoubleField:   schemapb.DataType_Double,
	//     testFloatVecField: schemapb.DataType_FloatVector,
	// }
	// if enableMultipleVectorFields {
	//     fieldName2Types[testBinaryVecField] = schemapb.DataType_BinaryVector
	// }
	// schema := constructCollectionSchemaByDataType(collectionName, fieldName2Types, testInt64Field, false)
	// marshaledSchema, err := proto.Marshal(schema)
	// assert.NoError(t, err)
	//
	// createColT := &createCollectionTask{
	//     Condition: NewTaskCondition(ctx),
	//     CreateCollectionRequest: &milvuspb.CreateCollectionRequest{
	//         Base:           nil,
	//         CollectionName: collectionName,
	//         Schema:         marshaledSchema,
	//         ShardsNum:      shardsNum,
	//     },
	//     ctx:       ctx,
	//     rootCoord: rc,
	//     result:    nil,
	//     schema:    nil,
	// }
	//
	// assert.NoError(t, createColT.OnEnqueue())
	// assert.NoError(t, createColT.PreExecute(ctx))
	// assert.NoError(t, createColT.Execute(ctx))
	// assert.NoError(t, createColT.PostExecute(ctx))
	//
	// dmlChannelsFunc := getDmlChannelsFunc(ctx, rc)
	// query := newMockGetChannelsService()
	// factory := newSimpleMockMsgStreamFactory()
	//
	// collectionID, err := globalMetaCache.GetCollectionID(ctx, collectionName)
	// assert.NoError(t, err)
	//
	// qc := NewQueryCoordMock()
	// qc.Start()
	// defer qc.Stop()
	// status, err := qc.LoadCollection(ctx, &querypb.LoadCollectionRequest{
	//     Base: &commonpb.MsgBase{
	//         MsgType:   commonpb.MsgType_LoadCollection,
	//         MsgID:     0,
	//         Timestamp: 0,
	//         SourceID:  paramtable.GetNodeID(),
	//     },
	//     DbID:         0,
	//     CollectionID: collectionID,
	//     Schema:       nil,
	// })
	// assert.NoError(t, err)
	// assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	//
	// req := constructSearchRequest("", collectionName,
	//     expr,
	//     testFloatVecField,
	//     nq, dim, nprobe, topk, roundDecimal)
	//
	// task := &searchTaskV2{
	//     Condition: NewTaskCondition(ctx),
	//     SearchRequest: &internalpb.SearchRequest{
	//         Base: &commonpb.MsgBase{
	//             MsgType:   commonpb.MsgType_Search,
	//             MsgID:     0,
	//             Timestamp: 0,
	//             SourceID:  paramtable.GetNodeID(),
	//         },
	//         ResultChannelID:    strconv.FormatInt(paramtable.GetNodeID(), 10),
	//         DbID:               0,
	//         CollectionID:       0,
	//         PartitionIDs:       nil,
	//         Dsl:                "",
	//         PlaceholderGroup:   nil,
	//         DslType:            0,
	//         SerializedExprPlan: nil,
	//         OutputFieldsId:     nil,
	//         TravelTimestamp:    0,
	//         GuaranteeTimestamp: 0,
	//     },
	//     ctx:       ctx,
	//     resultBuf: make(chan *internalpb.SearchResults, 10),
	//     result:    nil,
	//     request:   req,
	//     qc:        qc,
	//     tr:        timerecord.NewTimeRecorder("search"),
	// }
	//
	// // simple mock for query node
	// // TODO(dragondriver): should we replace this mock using RocksMq or MemMsgStream?
	//
	//
	// var wg sync.WaitGroup
	// wg.Add(1)
	// consumeCtx, cancel := context.WithCancel(ctx)
	// go func() {
	//     defer wg.Done()
	//     for {
	//         select {
	//         case <-consumeCtx.Done():
	//             return
	//         case pack, ok := <-stream.Chan():
	//             assert.True(t, ok)
	//             if pack == nil {
	//                 continue
	//             }
	//
	//             for _, msg := range pack.Msgs {
	//                 _, ok := msg.(*msgstream.SearchMsg)
	//                 assert.True(t, ok)
	//                 // TODO(dragondriver): construct result according to the request
	//
	//                 constructSearchResulstData := func() *schemapb.SearchResultData {
	//                     resultData := &schemapb.SearchResultData{
	//                         NumQueries: int64(nq),
	//                         TopK:       int64(topk),
	//                         Scores:     make([]float32, nq*topk),
	//                         Ids: &schemapb.IDs{
	//                             IdField: &schemapb.IDs_IntId{
	//                                 IntId: &schemapb.LongArray{
	//                                     Data: make([]int64, nq*topk),
	//                                 },
	//                             },
	//                         },
	//                         Topks: make([]int64, nq),
	//                     }
	//
	//                     fieldID := common.StartOfUserFieldID
	//                     for fieldName, dataType := range fieldName2Types {
	//                         resultData.FieldsData = append(resultData.FieldsData, generateFieldData(dataType, fieldName, int64(fieldID), nq*topk))
	//                         fieldID++
	//                     }
	//
	//                     for i := 0; i < nq; i++ {
	//                         for j := 0; j < topk; j++ {
	//                             offset := i*topk + j
	//                             score := float32(uniquegenerator.GetUniqueIntGeneratorIns().GetInt()) // increasingly
	//                             id := int64(uniquegenerator.GetUniqueIntGeneratorIns().GetInt())
	//                             resultData.Scores[offset] = score
	//                             resultData.Ids.IdField.(*schemapb.IDs_IntId).IntId.Data[offset] = id
	//                         }
	//                         resultData.Topks[i] = int64(topk)
	//                     }
	//
	//                     return resultData
	//                 }
	//
	//                 result1 := &internalpb.SearchResults{
	//                     Base: &commonpb.MsgBase{
	//                         MsgType:   commonpb.MsgType_SearchResult,
	//                         MsgID:     0,
	//                         Timestamp: 0,
	//                         SourceID:  0,
	//                     },
	//                     Status: &commonpb.Status{
	//                         ErrorCode: commonpb.ErrorCode_Success,
	//                         Reason:    "",
	//                     },
	//                     ResultChannelID:          "",
	//                     MetricType:               distance.L2,
	//                     NumQueries:               int64(nq),
	//                     TopK:                     int64(topk),
	//                     SealedSegmentIDsSearched: nil,
	//                     ChannelIDsSearched:       nil,
	//                     GlobalSealedSegmentIDs:   nil,
	//                     SlicedBlob:               nil,
	//                     SlicedNumCount:           1,
	//                     SlicedOffset:             0,
	//                 }
	//                 resultData := constructSearchResulstData()
	//                 sliceBlob, err := proto.Marshal(resultData)
	//                 assert.NoError(t, err)
	//                 result1.SlicedBlob = sliceBlob
	//
	//                 // result2.SliceBlob = nil, will be skipped in decode stage
	//                 result2 := &internalpb.SearchResults{
	//                     Base: &commonpb.MsgBase{
	//                         MsgType:   commonpb.MsgType_SearchResult,
	//                         MsgID:     0,
	//                         Timestamp: 0,
	//                         SourceID:  0,
	//                     },
	//                     Status: &commonpb.Status{
	//                         ErrorCode: commonpb.ErrorCode_Success,
	//                         Reason:    "",
	//                     },
	//                     ResultChannelID:          "",
	//                     MetricType:               distance.L2,
	//                     NumQueries:               int64(nq),
	//                     TopK:                     int64(topk),
	//                     SealedSegmentIDsSearched: nil,
	//                     ChannelIDsSearched:       nil,
	//                     GlobalSealedSegmentIDs:   nil,
	//                     SlicedBlob:               nil,
	//                     SlicedNumCount:           1,
	//                     SlicedOffset:             0,
	//                 }
	//
	//                 // send search result
	//                 task.resultBuf <- result1
	//                 task.resultBuf <- result2
	//             }
	//         }
	//     }
	// }()
	//
	// assert.NoError(t, task.OnEnqueue())
	// assert.Error(t, task.PreExecute(ctx))
	//
	// cancel()
	// wg.Wait()
}

func TestSearchTaskV2_all(t *testing.T) {
	// var err error
	//
	// Params.ProxyCfg.SearchResultChannelNames = []string{funcutil.GenRandomStr()}
	//
	// rc := NewRootCoordMock()
	// rc.Start()
	// defer rc.Stop()
	//
	// ctx := context.Background()
	//
	// err = InitMetaCache(ctx, rc)
	// assert.NoError(t, err)
	//
	// shardsNum := int32(2)
	// prefix := "TestSearchTaskV2_all"
	// collectionName := prefix + funcutil.GenRandomStr()
	//
	// dim := 128
	// expr := fmt.Sprintf("%s > 0", testInt64Field)
	// nq := 10
	// topk := 10
	// roundDecimal := 3
	// nprobe := 10
	//
	// fieldName2Types := map[string]schemapb.DataType{
	//     testBoolField:     schemapb.DataType_Bool,
	//     testInt32Field:    schemapb.DataType_Int32,
	//     testInt64Field:    schemapb.DataType_Int64,
	//     testFloatField:    schemapb.DataType_Float,
	//     testDoubleField:   schemapb.DataType_Double,
	//     testFloatVecField: schemapb.DataType_FloatVector,
	// }
	// if enableMultipleVectorFields {
	//     fieldName2Types[testBinaryVecField] = schemapb.DataType_BinaryVector
	// }
	//
	// schema := constructCollectionSchemaByDataType(collectionName, fieldName2Types, testInt64Field, false)
	// marshaledSchema, err := proto.Marshal(schema)
	// assert.NoError(t, err)
	//
	// createColT := &createCollectionTask{
	//     Condition: NewTaskCondition(ctx),
	//     CreateCollectionRequest: &milvuspb.CreateCollectionRequest{
	//         Base:           nil,
	//         CollectionName: collectionName,
	//         Schema:         marshaledSchema,
	//         ShardsNum:      shardsNum,
	//     },
	//     ctx:       ctx,
	//     rootCoord: rc,
	//     result:    nil,
	//     schema:    nil,
	// }
	//
	// assert.NoError(t, createColT.OnEnqueue())
	// assert.NoError(t, createColT.PreExecute(ctx))
	// assert.NoError(t, createColT.Execute(ctx))
	// assert.NoError(t, createColT.PostExecute(ctx))
	//
	// dmlChannelsFunc := getDmlChannelsFunc(ctx, rc)
	// query := newMockGetChannelsService()
	// factory := newSimpleMockMsgStreamFactory()
	//
	// collectionID, err := globalMetaCache.GetCollectionID(ctx, collectionName)
	// assert.NoError(t, err)
	//
	// qc := NewQueryCoordMock()
	// qc.Start()
	// defer qc.Stop()
	// status, err := qc.LoadCollection(ctx, &querypb.LoadCollectionRequest{
	//     Base: &commonpb.MsgBase{
	//         MsgType:   commonpb.MsgType_LoadCollection,
	//         MsgID:     0,
	//         Timestamp: 0,
	//         SourceID:  paramtable.GetNodeID(),
	//     },
	//     DbID:         0,
	//     CollectionID: collectionID,
	//     Schema:       nil,
	// })
	// assert.NoError(t, err)
	// assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	//
	// req := constructSearchRequest("", collectionName,
	//     expr,
	//     testFloatVecField,
	//     nq, dim, nprobe, topk, roundDecimal)
	//
	// task := &searchTaskV2{
	//     Condition: NewTaskCondition(ctx),
	//     SearchRequest: &internalpb.SearchRequest{
	//         Base: &commonpb.MsgBase{
	//             MsgType:   commonpb.MsgType_Search,
	//             MsgID:     0,
	//             Timestamp: 0,
	//             SourceID:  paramtable.GetNodeID(),
	//         },
	//         ResultChannelID:    strconv.FormatInt(paramtable.GetNodeID(), 10),
	//         DbID:               0,
	//         CollectionID:       0,
	//         PartitionIDs:       nil,
	//         Dsl:                "",
	//         PlaceholderGroup:   nil,
	//         DslType:            0,
	//         SerializedExprPlan: nil,
	//         OutputFieldsId:     nil,
	//         TravelTimestamp:    0,
	//         GuaranteeTimestamp: 0,
	//     },
	//     ctx:       ctx,
	//     resultBuf: make(chan *internalpb.SearchResults, 10),
	//     result:    nil,
	//     request:   req,
	//     qc:        qc,
	//     tr:        timerecord.NewTimeRecorder("search"),
	// }
	//
	// // simple mock for query node
	// // TODO(dragondriver): should we replace this mock using RocksMq or MemMsgStream?
	//
	// var wg sync.WaitGroup
	// wg.Add(1)
	// consumeCtx, cancel := context.WithCancel(ctx)
	// go func() {
	//     defer wg.Done()
	//     for {
	//         select {
	//         case <-consumeCtx.Done():
	//             return
	//         case pack, ok := <-stream.Chan():
	//             assert.True(t, ok)
	//             if pack == nil {
	//                 continue
	//             }
	//
	//             for _, msg := range pack.Msgs {
	//                 _, ok := msg.(*msgstream.SearchMsg)
	//                 assert.True(t, ok)
	//                 // TODO(dragondriver): construct result according to the request
	//
	//                 constructSearchResulstData := func() *schemapb.SearchResultData {
	//                     resultData := &schemapb.SearchResultData{
	//                         NumQueries: int64(nq),
	//                         TopK:       int64(topk),
	//                         Scores:     make([]float32, nq*topk),
	//                         Ids: &schemapb.IDs{
	//                             IdField: &schemapb.IDs_IntId{
	//                                 IntId: &schemapb.LongArray{
	//                                     Data: make([]int64, nq*topk),
	//                                 },
	//                             },
	//                         },
	//                         Topks: make([]int64, nq),
	//                     }
	//
	//                     fieldID := common.StartOfUserFieldID
	//                     for fieldName, dataType := range fieldName2Types {
	//                         resultData.FieldsData = append(resultData.FieldsData, generateFieldData(dataType, fieldName, int64(fieldID), nq*topk))
	//                         fieldID++
	//                     }
	//
	//                     for i := 0; i < nq; i++ {
	//                         for j := 0; j < topk; j++ {
	//                             offset := i*topk + j
	//                             score := float32(uniquegenerator.GetUniqueIntGeneratorIns().GetInt()) // increasingly
	//                             id := int64(uniquegenerator.GetUniqueIntGeneratorIns().GetInt())
	//                             resultData.Scores[offset] = score
	//                             resultData.Ids.IdField.(*schemapb.IDs_IntId).IntId.Data[offset] = id
	//                         }
	//                         resultData.Topks[i] = int64(topk)
	//                     }
	//
	//                     return resultData
	//                 }
	//
	//                 result1 := &internalpb.SearchResults{
	//                     Base: &commonpb.MsgBase{
	//                         MsgType:   commonpb.MsgType_SearchResult,
	//                         MsgID:     0,
	//                         Timestamp: 0,
	//                         SourceID:  0,
	//                     },
	//                     Status: &commonpb.Status{
	//                         ErrorCode: commonpb.ErrorCode_Success,
	//                         Reason:    "",
	//                     },
	//                     ResultChannelID:          "",
	//                     MetricType:               distance.L2,
	//                     NumQueries:               int64(nq),
	//                     TopK:                     int64(topk),
	//                     SealedSegmentIDsSearched: nil,
	//                     ChannelIDsSearched:       nil,
	//                     GlobalSealedSegmentIDs:   nil,
	//                     SlicedBlob:               nil,
	//                     SlicedNumCount:           1,
	//                     SlicedOffset:             0,
	//                 }
	//                 resultData := constructSearchResulstData()
	//                 sliceBlob, err := proto.Marshal(resultData)
	//                 assert.NoError(t, err)
	//                 result1.SlicedBlob = sliceBlob
	//
	//                 // result2.SliceBlob = nil, will be skipped in decode stage
	//                 result2 := &internalpb.SearchResults{
	//                     Base: &commonpb.MsgBase{
	//                         MsgType:   commonpb.MsgType_SearchResult,
	//                         MsgID:     0,
	//                         Timestamp: 0,
	//                         SourceID:  0,
	//                     },
	//                     Status: &commonpb.Status{
	//                         ErrorCode: commonpb.ErrorCode_Success,
	//                         Reason:    "",
	//                     },
	//                     ResultChannelID:          "",
	//                     MetricType:               distance.L2,
	//                     NumQueries:               int64(nq),
	//                     TopK:                     int64(topk),
	//                     SealedSegmentIDsSearched: nil,
	//                     ChannelIDsSearched:       nil,
	//                     GlobalSealedSegmentIDs:   nil,
	//                     SlicedBlob:               nil,
	//                     SlicedNumCount:           1,
	//                     SlicedOffset:             0,
	//                 }
	//
	//                 // send search result
	//                 task.resultBuf <- result1
	//                 task.resultBuf <- result2
	//             }
	//         }
	//     }
	// }()
	//
	// assert.NoError(t, task.OnEnqueue())
	// assert.NoError(t, task.PreExecute(ctx))
	// assert.NoError(t, task.Execute(ctx))
	// assert.NoError(t, task.PostExecute(ctx))
	//
	// cancel()
	// wg.Wait()
}

func TestSearchTaskV2_7803_reduce(t *testing.T) {
	// var err error
	//
	// Params.ProxyCfg.SearchResultChannelNames = []string{funcutil.GenRandomStr()}
	//
	// rc := NewRootCoordMock()
	// rc.Start()
	// defer rc.Stop()
	//
	// ctx := context.Background()
	//
	// err = InitMetaCache(ctx, rc)
	// assert.NoError(t, err)
	//
	// shardsNum := int32(2)
	// prefix := "TestSearchTaskV2_7803_reduce"
	// collectionName := prefix + funcutil.GenRandomStr()
	// int64Field := "int64"
	// floatVecField := "fvec"
	// dim := 128
	// expr := fmt.Sprintf("%s > 0", int64Field)
	// nq := 10
	// topk := 10
	// roundDecimal := 3
	// nprobe := 10
	//
	// schema := constructCollectionSchema(
	//     int64Field,
	//     floatVecField,
	//     dim,
	//     collectionName)
	// marshaledSchema, err := proto.Marshal(schema)
	// assert.NoError(t, err)
	//
	// createColT := &createCollectionTask{
	//     Condition: NewTaskCondition(ctx),
	//     CreateCollectionRequest: &milvuspb.CreateCollectionRequest{
	//         Base:           nil,
	//         CollectionName: collectionName,
	//         Schema:         marshaledSchema,
	//         ShardsNum:      shardsNum,
	//     },
	//     ctx:       ctx,
	//     rootCoord: rc,
	//     result:    nil,
	//     schema:    nil,
	// }
	//
	// assert.NoError(t, createColT.OnEnqueue())
	// assert.NoError(t, createColT.PreExecute(ctx))
	// assert.NoError(t, createColT.Execute(ctx))
	// assert.NoError(t, createColT.PostExecute(ctx))
	//
	// dmlChannelsFunc := getDmlChannelsFunc(ctx, rc)
	// query := newMockGetChannelsService()
	// factory := newSimpleMockMsgStreamFactory()
	//
	// collectionID, err := globalMetaCache.GetCollectionID(ctx, collectionName)
	// assert.NoError(t, err)
	//
	// qc := NewQueryCoordMock()
	// qc.Start()
	// defer qc.Stop()
	// status, err := qc.LoadCollection(ctx, &querypb.LoadCollectionRequest{
	//     Base: &commonpb.MsgBase{
	//         MsgType:   commonpb.MsgType_LoadCollection,
	//         MsgID:     0,
	//         Timestamp: 0,
	//         SourceID:  paramtable.GetNodeID(),
	//     },
	//     DbID:         0,
	//     CollectionID: collectionID,
	//     Schema:       nil,
	// })
	// assert.NoError(t, err)
	// assert.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)
	//
	// req := constructSearchRequest("", collectionName,
	//     expr,
	//     floatVecField,
	//     nq, dim, nprobe, topk, roundDecimal)
	//
	// task := &searchTaskV2{
	//     Condition: NewTaskCondition(ctx),
	//     SearchRequest: &internalpb.SearchRequest{
	//         Base: &commonpb.MsgBase{
	//             MsgType:   commonpb.MsgType_Search,
	//             MsgID:     0,
	//             Timestamp: 0,
	//             SourceID:  paramtable.GetNodeID(),
	//         },
	//         ResultChannelID:    strconv.FormatInt(paramtable.GetNodeID(), 10),
	//         DbID:               0,
	//         CollectionID:       0,
	//         PartitionIDs:       nil,
	//         Dsl:                "",
	//         PlaceholderGroup:   nil,
	//         DslType:            0,
	//         SerializedExprPlan: nil,
	//         OutputFieldsId:     nil,
	//         TravelTimestamp:    0,
	//         GuaranteeTimestamp: 0,
	//     },
	//     ctx:       ctx,
	//     resultBuf: make(chan *internalpb.SearchResults, 10),
	//     result:    nil,
	//     request:   req,
	//     qc:        qc,
	//     tr:        timerecord.NewTimeRecorder("search"),
	// }
	//
	// // simple mock for query node
	// // TODO(dragondriver): should we replace this mock using RocksMq or MemMsgStream?
	//
	// var wg sync.WaitGroup
	// wg.Add(1)
	// consumeCtx, cancel := context.WithCancel(ctx)
	// go func() {
	//     defer wg.Done()
	//     for {
	//         select {
	//         case <-consumeCtx.Done():
	//             return
	//         case pack, ok := <-stream.Chan():
	//             assert.True(t, ok)
	//             if pack == nil {
	//                 continue
	//             }
	//
	//             for _, msg := range pack.Msgs {
	//                 _, ok := msg.(*msgstream.SearchMsg)
	//                 assert.True(t, ok)
	//                 // TODO(dragondriver): construct result according to the request
	//
	//                 constructSearchResulstData := func(invalidNum int) *schemapb.SearchResultData {
	//                     resultData := &schemapb.SearchResultData{
	//                         NumQueries: int64(nq),
	//                         TopK:       int64(topk),
	//                         FieldsData: nil,
	//                         Scores:     make([]float32, nq*topk),
	//                         Ids: &schemapb.IDs{
	//                             IdField: &schemapb.IDs_IntId{
	//                                 IntId: &schemapb.LongArray{
	//                                     Data: make([]int64, nq*topk),
	//                                 },
	//                             },
	//                         },
	//                         Topks: make([]int64, nq),
	//                     }
	//
	//                     for i := 0; i < nq; i++ {
	//                         for j := 0; j < topk; j++ {
	//                             offset := i*topk + j
	//                             if j >= invalidNum {
	//                                 resultData.Scores[offset] = minFloat32
	//                                 resultData.Ids.IdField.(*schemapb.IDs_IntId).IntId.Data[offset] = -1
	//                             } else {
	//                                 score := float32(uniquegenerator.GetUniqueIntGeneratorIns().GetInt()) // increasingly
	//                                 id := int64(uniquegenerator.GetUniqueIntGeneratorIns().GetInt())
	//                                 resultData.Scores[offset] = score
	//                                 resultData.Ids.IdField.(*schemapb.IDs_IntId).IntId.Data[offset] = id
	//                             }
	//                         }
	//                         resultData.Topks[i] = int64(topk)
	//                     }
	//
	//                     return resultData
	//                 }
	//
	//                 result1 := &internalpb.SearchResults{
	//                     Base: &commonpb.MsgBase{
	//                         MsgType:   commonpb.MsgType_SearchResult,
	//                         MsgID:     0,
	//                         Timestamp: 0,
	//                         SourceID:  0,
	//                     },
	//                     Status: &commonpb.Status{
	//                         ErrorCode: commonpb.ErrorCode_Success,
	//                         Reason:    "",
	//                     },
	//                     ResultChannelID:          "",
	//                     MetricType:               distance.L2,
	//                     NumQueries:               int64(nq),
	//                     TopK:                     int64(topk),
	//                     SealedSegmentIDsSearched: nil,
	//                     ChannelIDsSearched:       nil,
	//                     GlobalSealedSegmentIDs:   nil,
	//                     SlicedBlob:               nil,
	//                     SlicedNumCount:           1,
	//                     SlicedOffset:             0,
	//                 }
	//                 resultData := constructSearchResulstData(topk / 2)
	//                 sliceBlob, err := proto.Marshal(resultData)
	//                 assert.NoError(t, err)
	//                 result1.SlicedBlob = sliceBlob
	//
	//                 result2 := &internalpb.SearchResults{
	//                     Base: &commonpb.MsgBase{
	//                         MsgType:   commonpb.MsgType_SearchResult,
	//                         MsgID:     0,
	//                         Timestamp: 0,
	//                         SourceID:  0,
	//                     },
	//                     Status: &commonpb.Status{
	//                         ErrorCode: commonpb.ErrorCode_Success,
	//                         Reason:    "",
	//                     },
	//                     ResultChannelID:          "",
	//                     MetricType:               distance.L2,
	//                     NumQueries:               int64(nq),
	//                     TopK:                     int64(topk),
	//                     SealedSegmentIDsSearched: nil,
	//                     ChannelIDsSearched:       nil,
	//                     GlobalSealedSegmentIDs:   nil,
	//                     SlicedBlob:               nil,
	//                     SlicedNumCount:           1,
	//                     SlicedOffset:             0,
	//                 }
	//                 resultData2 := constructSearchResulstData(topk - topk/2)
	//                 sliceBlob2, err := proto.Marshal(resultData2)
	//                 assert.NoError(t, err)
	//                 result2.SlicedBlob = sliceBlob2
	//
	//                 // send search result
	//                 task.resultBuf <- result1
	//                 task.resultBuf <- result2
	//             }
	//         }
	//     }
	// }()
	//
	// assert.NoError(t, task.OnEnqueue())
	// assert.NoError(t, task.PreExecute(ctx))
	// assert.NoError(t, task.Execute(ctx))
	// assert.NoError(t, task.PostExecute(ctx))
	//
	// cancel()
	// wg.Wait()
}

func Test_checkSearchResultData(t *testing.T) {
	type args struct {
		data *schemapb.SearchResultData
		nq   int64
		topk int64
	}
	tests := []struct {
		description string
		wantErr     bool

		args args
	}{
		{
			"data.NumQueries != nq", true,
			args{
				data: &schemapb.SearchResultData{NumQueries: 100},
				nq:   10,
			},
		},
		{
			"data.TopK != topk", true,
			args{
				data: &schemapb.SearchResultData{NumQueries: 1, TopK: 1},
				nq:   1,
				topk: 10,
			},
		},
		{
			"size of IntId != NumQueries * TopK", true,
			args{
				data: &schemapb.SearchResultData{
					NumQueries: 1,
					TopK:       1,
					Ids: &schemapb.IDs{
						IdField: &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: []int64{1, 2}}},
					},
				},
				nq:   1,
				topk: 1,
			},
		},
		{
			"size of StrID != NumQueries * TopK", true,
			args{
				data: &schemapb.SearchResultData{
					NumQueries: 1,
					TopK:       1,
					Ids: &schemapb.IDs{
						IdField: &schemapb.IDs_StrId{StrId: &schemapb.StringArray{Data: []string{"1", "2"}}},
					},
				},
				nq:   1,
				topk: 1,
			},
		},
		{
			"size of score != nq * topK", true,
			args{
				data: &schemapb.SearchResultData{
					NumQueries: 1,
					TopK:       1,
					Ids: &schemapb.IDs{
						IdField: &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: []int64{1}}},
					},
					Scores: []float32{0.99, 0.98},
				},
				nq:   1,
				topk: 1,
			},
		},
		{
			"correct params", false,
			args{
				data: &schemapb.SearchResultData{
					NumQueries: 1,
					TopK:       1,
					Ids: &schemapb.IDs{
						IdField: &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: []int64{1}}},
					},
					Scores: []float32{0.99},
				},
				nq:   1,
				topk: 1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			pkLength := typeutil.GetSizeOfIDs(test.args.data.GetIds())
			err := checkSearchResultData(test.args.data, test.args.nq, test.args.topk, pkLength)

			if test.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTaskSearch_selectHighestScoreIndex(t *testing.T) {
	t.Run("Integer ID", func(t *testing.T) {
		type args struct {
			subSearchResultData []*schemapb.SearchResultData
			subSearchNqOffset   [][]int64
			cursors             []int64
			topk                int64
			nq                  int64
		}
		tests := []struct {
			description string
			args        args

			expectedIdx     []int
			expectedDataIdx []int
		}{
			{
				description: "reduce 2 subSearchResultData",
				args: args{
					subSearchResultData: []*schemapb.SearchResultData{
						{
							Ids: &schemapb.IDs{
								IdField: &schemapb.IDs_IntId{
									IntId: &schemapb.LongArray{
										Data: []int64{11, 9, 8, 5, 3, 1},
									},
								},
							},
							Scores: []float32{1.1, 0.9, 0.8, 0.5, 0.3, 0.1},
							Topks:  []int64{2, 2, 2},
						},
						{
							Ids: &schemapb.IDs{
								IdField: &schemapb.IDs_IntId{
									IntId: &schemapb.LongArray{
										Data: []int64{12, 10, 7, 6, 4, 2},
									},
								},
							},
							Scores: []float32{1.2, 1.0, 0.7, 0.5, 0.4, 0.2},
							Topks:  []int64{2, 2, 2},
						},
					},
					subSearchNqOffset: [][]int64{{0, 2, 4}, {0, 2, 4}},
					cursors:           []int64{0, 0},
					topk:              2,
					nq:                3,
				},
				expectedIdx:     []int{1, 0, 1},
				expectedDataIdx: []int{0, 2, 4},
			},
		}
		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				for nqNum := int64(0); nqNum < test.args.nq; nqNum++ {
					idx, dataIdx := selectHighestScoreIndex(context.TODO(), test.args.subSearchResultData, test.args.subSearchNqOffset, test.args.cursors, nqNum)
					assert.Equal(t, test.expectedIdx[nqNum], idx)
					assert.Equal(t, test.expectedDataIdx[nqNum], int(dataIdx))
				}
			})
		}
	})

	t.Run("String ID", func(t *testing.T) {
		type args struct {
			subSearchResultData []*schemapb.SearchResultData
			subSearchNqOffset   [][]int64
			cursors             []int64
			topk                int64
			nq                  int64
		}
		tests := []struct {
			description string
			args        args

			expectedIdx     []int
			expectedDataIdx []int
		}{
			{
				description: "reduce 2 subSearchResultData",
				args: args{
					subSearchResultData: []*schemapb.SearchResultData{
						{
							Ids: &schemapb.IDs{
								IdField: &schemapb.IDs_StrId{
									StrId: &schemapb.StringArray{
										Data: []string{"11", "9", "8", "5", "3", "1"},
									},
								},
							},
							Scores: []float32{1.1, 0.9, 0.8, 0.5, 0.3, 0.1},
							Topks:  []int64{2, 2, 2},
						},
						{
							Ids: &schemapb.IDs{
								IdField: &schemapb.IDs_StrId{
									StrId: &schemapb.StringArray{
										Data: []string{"12", "10", "7", "6", "4", "2"},
									},
								},
							},
							Scores: []float32{1.2, 1.0, 0.7, 0.5, 0.4, 0.2},
							Topks:  []int64{2, 2, 2},
						},
					},
					subSearchNqOffset: [][]int64{{0, 2, 4}, {0, 2, 4}},
					cursors:           []int64{0, 0},
					topk:              2,
					nq:                3,
				},
				expectedIdx:     []int{1, 0, 1},
				expectedDataIdx: []int{0, 2, 4},
			},
		}
		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				for nqNum := int64(0); nqNum < test.args.nq; nqNum++ {
					idx, dataIdx := selectHighestScoreIndex(context.TODO(), test.args.subSearchResultData, test.args.subSearchNqOffset, test.args.cursors, nqNum)
					assert.Equal(t, test.expectedIdx[nqNum], idx)
					assert.Equal(t, test.expectedDataIdx[nqNum], int(dataIdx))
				}
			})
		}
	})
}

func TestTaskSearch_reduceSearchResultData(t *testing.T) {
	var (
		topk int64 = 5
		nq   int64 = 2
	)

	data := [][]int64{
		{10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
		{20, 19, 18, 17, 16, 15, 14, 13, 12, 11},
		{30, 29, 28, 27, 26, 25, 24, 23, 22, 21},
		{40, 39, 38, 37, 36, 35, 34, 33, 32, 31},
		{50, 49, 48, 47, 46, 45, 44, 43, 42, 41},
	}

	score := [][]float32{
		{10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
		{20, 19, 18, 17, 16, 15, 14, 13, 12, 11},
		{30, 29, 28, 27, 26, 25, 24, 23, 22, 21},
		{40, 39, 38, 37, 36, 35, 34, 33, 32, 31},
		{50, 49, 48, 47, 46, 45, 44, 43, 42, 41},
	}

	resultScore := []float32{-50, -49, -48, -47, -46, -45, -44, -43, -42, -41}

	t.Run("Offset limit", func(t *testing.T) {
		tests := []struct {
			description string
			offset      int64
			limit       int64

			outScore []float32
			outData  []int64
		}{
			{
				"offset 0, limit 5", 0, 5,
				[]float32{-50, -49, -48, -47, -46, -45, -44, -43, -42, -41},
				[]int64{50, 49, 48, 47, 46, 45, 44, 43, 42, 41},
			},
			{
				"offset 1, limit 4", 1, 4,
				[]float32{-49, -48, -47, -46, -44, -43, -42, -41},
				[]int64{49, 48, 47, 46, 44, 43, 42, 41},
			},
			{
				"offset 2, limit 3", 2, 3,
				[]float32{-48, -47, -46, -43, -42, -41},
				[]int64{48, 47, 46, 43, 42, 41},
			},
			{
				"offset 3, limit 2", 3, 2,
				[]float32{-47, -46, -42, -41},
				[]int64{47, 46, 42, 41},
			},
			{
				"offset 4, limit 1", 4, 1,
				[]float32{-46, -41},
				[]int64{46, 41},
			},
		}

		var results []*schemapb.SearchResultData
		for i := range data {
			r := getSearchResultData(nq, topk)

			r.Ids.IdField = &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: data[i]}}
			r.Scores = score[i]
			r.Topks = []int64{5, 5}

			results = append(results, r)
		}

		queryInfo := &planpb.QueryInfo{
			GroupByFieldId: -1,
		}
		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				reduced, err := reduceSearchResult(context.TODO(), results,
					reduce.NewReduceSearchResultInfo(nq, topk).WithMetricType(metric.L2).WithPkType(schemapb.DataType_Int64).
						WithOffset(test.offset).WithGroupByField(queryInfo.GetGroupByFieldId()).WithGroupSize(queryInfo.GetGroupSize()))
				assert.NoError(t, err)
				assert.Equal(t, test.outData, reduced.GetResults().GetIds().GetIntId().GetData())
				assert.Equal(t, []int64{test.limit, test.limit}, reduced.GetResults().GetTopks())
				assert.Equal(t, test.limit, reduced.GetResults().GetTopK())
				assert.InDeltaSlice(t, test.outScore, reduced.GetResults().GetScores(), 10e-8)
			})
		}

		lessThanLimitTests := []struct {
			description string
			offset      int64
			limit       int64

			outLimit int64
			outScore []float32
			outData  []int64
		}{
			{
				"offset 0, limit 6", 0, 6, 5,
				[]float32{-50, -49, -48, -47, -46, -45, -44, -43, -42, -41},
				[]int64{50, 49, 48, 47, 46, 45, 44, 43, 42, 41},
			},
			{
				"offset 1, limit 5", 1, 5, 4,
				[]float32{-49, -48, -47, -46, -44, -43, -42, -41},
				[]int64{49, 48, 47, 46, 44, 43, 42, 41},
			},
			{
				"offset 2, limit 4", 2, 4, 3,
				[]float32{-48, -47, -46, -43, -42, -41},
				[]int64{48, 47, 46, 43, 42, 41},
			},
			{
				"offset 3, limit 3", 3, 3, 2,
				[]float32{-47, -46, -42, -41},
				[]int64{47, 46, 42, 41},
			},
			{
				"offset 4, limit 2", 4, 2, 1,
				[]float32{-46, -41},
				[]int64{46, 41},
			},
			{
				"offset 5, limit 1", 5, 1, 0,
				[]float32{},
				[]int64{},
			},
		}
		for _, test := range lessThanLimitTests {
			t.Run(test.description, func(t *testing.T) {
				reduced, err := reduceSearchResult(context.TODO(), results,
					reduce.NewReduceSearchResultInfo(nq, topk).WithMetricType(metric.L2).WithPkType(schemapb.DataType_Int64).WithOffset(test.offset).
						WithGroupByField(queryInfo.GetGroupByFieldId()).WithGroupSize(queryInfo.GetGroupSize()))
				assert.NoError(t, err)
				assert.Equal(t, test.outData, reduced.GetResults().GetIds().GetIntId().GetData())
				assert.Equal(t, []int64{test.outLimit, test.outLimit}, reduced.GetResults().GetTopks())
				assert.Equal(t, test.outLimit, reduced.GetResults().GetTopK())
				assert.InDeltaSlice(t, test.outScore, reduced.GetResults().GetScores(), 10e-8)
			})
		}
	})

	t.Run("Int64 ID", func(t *testing.T) {
		resultData := []int64{50, 49, 48, 47, 46, 45, 44, 43, 42, 41}

		var results []*schemapb.SearchResultData
		for i := range data {
			r := getSearchResultData(nq, topk)

			r.Ids.IdField = &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: data[i]}}
			r.Scores = score[i]
			r.Topks = []int64{5, 5}

			results = append(results, r)
		}

		queryInfo := &planpb.QueryInfo{
			GroupByFieldId: -1,
		}

		reduced, err := reduceSearchResult(context.TODO(), results,
			reduce.NewReduceSearchResultInfo(nq, topk).WithMetricType(metric.L2).WithPkType(schemapb.DataType_Int64).WithGroupByField(queryInfo.GetGroupByFieldId()).WithGroupSize(queryInfo.GetGroupSize()))
		assert.NoError(t, err)
		assert.Equal(t, resultData, reduced.GetResults().GetIds().GetIntId().GetData())
		assert.Equal(t, []int64{5, 5}, reduced.GetResults().GetTopks())
		assert.Equal(t, int64(5), reduced.GetResults().GetTopK())
		assert.InDeltaSlice(t, resultScore, reduced.GetResults().GetScores(), 10e-8)
	})

	t.Run("String ID", func(t *testing.T) {
		resultData := []string{"50", "49", "48", "47", "46", "45", "44", "43", "42", "41"}

		var results []*schemapb.SearchResultData
		for i := range data {
			r := getSearchResultData(nq, topk)

			var strData []string
			for _, d := range data[i] {
				strData = append(strData, strconv.FormatInt(d, 10))
			}
			r.Ids.IdField = &schemapb.IDs_StrId{StrId: &schemapb.StringArray{Data: strData}}
			r.Scores = score[i]
			r.Topks = []int64{5, 5}

			results = append(results, r)
		}
		queryInfo := &planpb.QueryInfo{
			GroupByFieldId: -1,
		}
		reduced, err := reduceSearchResult(context.TODO(), results,
			reduce.NewReduceSearchResultInfo(nq, topk).WithMetricType(metric.L2).WithPkType(schemapb.DataType_VarChar).WithGroupByField(queryInfo.GetGroupByFieldId()).WithGroupSize(queryInfo.GetGroupSize()))

		assert.NoError(t, err)
		assert.Equal(t, resultData, reduced.GetResults().GetIds().GetStrId().GetData())
		assert.Equal(t, []int64{5, 5}, reduced.GetResults().GetTopks())
		assert.Equal(t, int64(5), reduced.GetResults().GetTopK())
		assert.InDeltaSlice(t, resultScore, reduced.GetResults().GetScores(), 10e-8)
	})
}

func TestTaskSearch_reduceGroupBySearchResultData(t *testing.T) {
	var (
		nq   int64 = 2
		topK int64 = 5
	)
	ids := [][]int64{
		{1, 3, 5, 7, 9, 1, 3, 5, 7, 9},
		{2, 4, 6, 8, 10, 2, 4, 6, 8, 10},
	}
	scores := [][]float32{
		{10, 8, 6, 4, 2, 10, 8, 6, 4, 2},
		{9, 7, 5, 3, 1, 9, 7, 5, 3, 1},
	}

	makePartialResult := func(ids []int64, scores []float32, groupByValues []int64, valids []bool) *schemapb.SearchResultData {
		result := getSearchResultData(nq, topK)
		result.Ids.IdField = &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: ids}}
		result.Scores = scores
		result.Topks = []int64{topK, topK}
		result.GroupByFieldValue = &schemapb.FieldData{
			Type: schemapb.DataType_Int64,
			Field: &schemapb.FieldData_Scalars{
				Scalars: &schemapb.ScalarField{
					Data: &schemapb.ScalarField_LongData{
						LongData: &schemapb.LongArray{
							Data: groupByValues,
						},
					},
				},
			},
			ValidData: valids,
		}
		return result
	}

	tests := []struct {
		name                  string
		inputs                []*schemapb.SearchResultData
		expectedIDs           []int64
		expectedScores        []float32
		expectedGroupByValues *schemapb.FieldData
	}{
		{
			name: "same group_by values",
			inputs: []*schemapb.SearchResultData{
				makePartialResult(ids[0], scores[0], []int64{1, 2, 3, 4, 5, 1, 2, 3, 4, 5}, nil),
				makePartialResult(ids[1], scores[1], []int64{1, 2, 3, 4, 5, 1, 2, 3, 4, 5}, nil),
			},
			expectedIDs:    []int64{1, 3, 5, 7, 9, 1, 3, 5, 7, 9},
			expectedScores: []float32{-10, -8, -6, -4, -2, -10, -8, -6, -4, -2},
			expectedGroupByValues: &schemapb.FieldData{
				Type: schemapb.DataType_Int64,
				Field: &schemapb.FieldData_Scalars{
					Scalars: &schemapb.ScalarField{
						Data: &schemapb.ScalarField_LongData{LongData: &schemapb.LongArray{Data: []int64{1, 2, 3, 4, 5, 1, 2, 3, 4, 5}}},
					},
				},
			},
		},
		{
			name: "different group_by values",
			inputs: []*schemapb.SearchResultData{
				makePartialResult(ids[0], scores[0], []int64{1, 2, 3, 4, 5, 1, 2, 3, 4, 5}, nil),
				makePartialResult(ids[1], scores[1], []int64{6, 8, 3, 4, 5, 6, 8, 3, 4, 5}, nil),
			},
			expectedIDs:    []int64{1, 2, 3, 4, 5, 1, 2, 3, 4, 5},
			expectedScores: []float32{-10, -9, -8, -7, -6, -10, -9, -8, -7, -6},
			expectedGroupByValues: &schemapb.FieldData{
				Type: schemapb.DataType_Int64,
				Field: &schemapb.FieldData_Scalars{
					Scalars: &schemapb.ScalarField{
						Data: &schemapb.ScalarField_LongData{LongData: &schemapb.LongArray{Data: []int64{1, 6, 2, 8, 3, 1, 6, 2, 8, 3}}},
					},
				},
			},
		},
		{
			name: "nullable group_by values",
			inputs: []*schemapb.SearchResultData{
				makePartialResult(ids[0], scores[0], []int64{1, 2, 3, 4, 1, 2, 3, 4}, []bool{true, true, true, true, false, true, true, true, true, false}),
				makePartialResult(ids[1], scores[1], []int64{1, 2, 3, 4, 1, 2, 3, 4}, []bool{true, true, true, true, false, true, true, true, true, false}),
			},
			expectedIDs:    []int64{1, 3, 5, 7, 9, 1, 3, 5, 7, 9},
			expectedScores: []float32{-10, -8, -6, -4, -2, -10, -8, -6, -4, -2},
			expectedGroupByValues: &schemapb.FieldData{
				Type: schemapb.DataType_Int64,
				Field: &schemapb.FieldData_Scalars{
					Scalars: &schemapb.ScalarField{
						Data: &schemapb.ScalarField_LongData{
							LongData: &schemapb.LongArray{Data: []int64{1, 2, 3, 4, 0, 1, 2, 3, 4, 0}},
						},
					},
				},
				ValidData: []bool{true, true, true, true, false, true, true, true, true, false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queryInfo := &planpb.QueryInfo{
				GroupByFieldId: 1,
				GroupSize:      1,
			}
			reduced, err := reduceSearchResult(context.TODO(), tt.inputs,
				reduce.NewReduceSearchResultInfo(nq, topK).
					WithMetricType(metric.L2).
					WithPkType(schemapb.DataType_Int64).
					WithGroupByField(queryInfo.GetGroupByFieldId()).
					WithGroupSize(queryInfo.GetGroupSize()))
			resultIDs := reduced.GetResults().GetIds().GetIntId().Data
			resultScores := reduced.GetResults().GetScores()
			resultGroupByValues := reduced.GetResults().GetGroupByFieldValue()
			assert.EqualValues(t, tt.expectedIDs, resultIDs)
			assert.EqualValues(t, tt.expectedScores, resultScores)
			assert.EqualValues(t, tt.expectedGroupByValues, resultGroupByValues)
			assert.NoError(t, err)
		})
	}
}

func TestTaskSearch_reduceGroupBySearchResultDataWithOffset(t *testing.T) {
	var (
		nq     int64 = 1
		limit  int64 = 5
		offset int64 = 5
	)
	ids := [][]int64{
		{1, 3, 5, 7, 9},
		{2, 4, 6, 8, 10},
	}
	scores := [][]float32{
		{10, 8, 6, 4, 2},
		{9, 7, 5, 3, 1},
	}
	groupByValuesArr := [][]int64{
		{1, 3, 5, 7, 9},
		{2, 4, 6, 8, 10},
	}
	expectedIDs := []int64{6, 7, 8, 9, 10}
	expectedScores := []float32{-5, -4, -3, -2, -1}
	expectedGroupByValues := []int64{6, 7, 8, 9, 10}

	var results []*schemapb.SearchResultData
	for j := range ids {
		result := getSearchResultData(nq, limit+offset)
		result.Ids.IdField = &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: ids[j]}}
		result.Scores = scores[j]
		result.Topks = []int64{limit}
		result.GroupByFieldValue = &schemapb.FieldData{
			Type: schemapb.DataType_Int64,
			Field: &schemapb.FieldData_Scalars{
				Scalars: &schemapb.ScalarField{
					Data: &schemapb.ScalarField_LongData{
						LongData: &schemapb.LongArray{
							Data: groupByValuesArr[j],
						},
					},
				},
			},
		}
		results = append(results, result)
	}

	queryInfo := &planpb.QueryInfo{
		GroupByFieldId: 1,
		GroupSize:      1,
	}
	reduced, err := reduceSearchResult(context.TODO(), results,
		reduce.NewReduceSearchResultInfo(nq, limit+offset).WithMetricType(metric.L2).WithPkType(schemapb.DataType_Int64).WithOffset(offset).WithGroupByField(queryInfo.GetGroupByFieldId()).WithGroupSize(queryInfo.GetGroupSize()))
	resultIDs := reduced.GetResults().GetIds().GetIntId().Data
	resultScores := reduced.GetResults().GetScores()
	resultGroupByValues := reduced.GetResults().GetGroupByFieldValue().GetScalars().GetLongData().GetData()
	assert.EqualValues(t, expectedIDs, resultIDs)
	assert.EqualValues(t, expectedScores, resultScores)
	assert.EqualValues(t, expectedGroupByValues, resultGroupByValues)
	assert.NoError(t, err)
}

func TestTaskSearch_reduceGroupBySearchWithGroupSizeMoreThanOne(t *testing.T) {
	var (
		nq   int64 = 2
		topK int64 = 5
	)
	ids := [][]int64{
		{1, 3, 5, 7, 9, 1, 3, 5, 7, 9},
		{2, 4, 6, 8, 10, 2, 4, 6, 8, 10},
	}
	scores := [][]float32{
		{10, 8, 6, 4, 2, 10, 8, 6, 4, 2},
		{9, 7, 5, 3, 1, 9, 7, 5, 3, 1},
	}

	groupByValuesArr := [][][]int64{
		{
			{1, 2, 3, 4, 5, 1, 2, 3, 4, 5},
			{1, 2, 3, 4, 5, 1, 2, 3, 4, 5},
		},
		{
			{1, 2, 3, 4, 5, 1, 2, 3, 4, 5},
			{6, 8, 3, 4, 5, 6, 8, 3, 4, 5},
		},
	}
	expectedIDs := [][]int64{
		{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		{1, 2, 3, 4, 5, 6, 1, 2, 3, 4, 5, 6},
	}
	expectedScores := [][]float32{
		{-10, -9, -8, -7, -6, -5, -4, -3, -2, -1, -10, -9, -8, -7, -6, -5, -4, -3, -2, -1},
		{-10, -9, -8, -7, -6, -5, -10, -9, -8, -7, -6, -5},
	}
	expectedGroupByValues := [][]int64{
		{1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5},
		{1, 6, 2, 8, 3, 3, 1, 6, 2, 8, 3, 3},
	}

	for i, groupByValues := range groupByValuesArr {
		t.Run("Group By correctness", func(t *testing.T) {
			var results []*schemapb.SearchResultData
			for j := range ids {
				result := getSearchResultData(nq, topK)
				result.Ids.IdField = &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: ids[j]}}
				result.Scores = scores[j]
				result.Topks = []int64{topK, topK}
				result.GroupByFieldValue = &schemapb.FieldData{
					Type: schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: groupByValues[j],
								},
							},
						},
					},
				}
				results = append(results, result)
			}
			queryInfo := &planpb.QueryInfo{
				GroupByFieldId: 1,
				GroupSize:      2,
			}
			reduced, err := reduceSearchResult(context.TODO(), results,
				reduce.NewReduceSearchResultInfo(nq, topK).WithMetricType(metric.L2).WithPkType(schemapb.DataType_Int64).WithGroupByField(queryInfo.GetGroupByFieldId()).WithGroupSize(queryInfo.GetGroupSize()))

			resultIDs := reduced.GetResults().GetIds().GetIntId().Data
			resultScores := reduced.GetResults().GetScores()
			resultGroupByValues := reduced.GetResults().GetGroupByFieldValue().GetScalars().GetLongData().GetData()
			assert.EqualValues(t, expectedIDs[i], resultIDs)
			assert.EqualValues(t, expectedScores[i], resultScores)
			assert.EqualValues(t, expectedGroupByValues[i], resultGroupByValues)
			assert.NoError(t, err)
		})
	}
}

func TestTaskSearch_reduceAdvanceSearchGroupBy(t *testing.T) {
	groupByField := int64(101)
	nq := int64(1)
	subSearchResultData := make([]*schemapb.SearchResultData, 0)
	topK := int64(3)
	{
		scores := []float32{0.9, 0.7, 0.65, 0.55, 0.52, 0.51, 0.5, 0.45, 0.43}
		ids := []int64{7, 5, 6, 11, 22, 14, 31, 23, 37}
		tops := []int64{9}
		groupFieldValue := []string{"aaa", "bbb", "ccc", "bbb", "bbb", "ccc", "aaa", "ccc", "aaa"}
		groupByVals := getFieldData("string", groupByField, schemapb.DataType_VarChar, groupFieldValue, 1)
		result1 := &schemapb.SearchResultData{
			Scores: scores,
			TopK:   topK,
			Ids: &schemapb.IDs{
				IdField: &schemapb.IDs_IntId{
					IntId: &schemapb.LongArray{
						Data: ids,
					},
				},
			},
			NumQueries:        nq,
			Topks:             tops,
			GroupByFieldValue: groupByVals,
		}
		subSearchResultData = append(subSearchResultData, result1)
	}
	{
		scores := []float32{0.83, 0.72, 0.72, 0.65, 0.63, 0.55, 0.52, 0.51, 0.48}
		ids := []int64{17, 15, 16, 21, 32, 24, 41, 33, 27}
		tops := []int64{9}
		groupFieldValue := []string{"xxx", "bbb", "ddd", "bbb", "bbb", "ddd", "xxx", "ddd", "xxx"}
		groupByVals := getFieldData("string", groupByField, schemapb.DataType_VarChar, groupFieldValue, 1)
		result2 := &schemapb.SearchResultData{
			TopK:   topK,
			Scores: scores,
			Ids: &schemapb.IDs{
				IdField: &schemapb.IDs_IntId{
					IntId: &schemapb.LongArray{
						Data: ids,
					},
				},
			},
			Topks:             tops,
			NumQueries:        nq,
			GroupByFieldValue: groupByVals,
		}
		subSearchResultData = append(subSearchResultData, result2)
	}
	groupSize := int64(3)

	reducedRes, err := reduceSearchResult(context.Background(), subSearchResultData,
		reduce.NewReduceSearchResultInfo(nq, topK).WithMetricType(metric.IP).WithPkType(schemapb.DataType_Int64).WithGroupByField(groupByField).WithGroupSize(groupSize).WithAdvance(true))
	assert.NoError(t, err)
	// reduce_advance_groupby will only merge results from different delegator without reducing any result
	assert.Equal(t, 18, len(reducedRes.GetResults().Ids.GetIntId().Data))
	assert.Equal(t, 18, len(reducedRes.GetResults().GetScores()))
	assert.Equal(t, 18, len(reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data))
	assert.Equal(t, topK, reducedRes.GetResults().GetTopK())
	assert.Equal(t, []int64{18}, reducedRes.GetResults().GetTopks())

	assert.Equal(t, []int64{7, 5, 6, 11, 22, 14, 31, 23, 37, 17, 15, 16, 21, 32, 24, 41, 33, 27}, reducedRes.GetResults().Ids.GetIntId().Data)
	assert.Equal(t, []float32{0.9, 0.7, 0.65, 0.55, 0.52, 0.51, 0.5, 0.45, 0.43, 0.83, 0.72, 0.72, 0.65, 0.63, 0.55, 0.52, 0.51, 0.48}, reducedRes.GetResults().GetScores())
	assert.Equal(t, []string{"aaa", "bbb", "ccc", "bbb", "bbb", "ccc", "aaa", "ccc", "aaa", "xxx", "bbb", "ddd", "bbb", "bbb", "ddd", "xxx", "ddd", "xxx"}, reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data)
}

func TestTaskSearch_reduceAdvanceSearchGroupByShortCut(t *testing.T) {
	groupByField := int64(101)
	nq := int64(1)
	subSearchResultData := make([]*schemapb.SearchResultData, 0)
	topK := int64(3)
	{
		scores := []float32{0.9, 0.7, 0.65, 0.55, 0.52, 0.51, 0.5, 0.45, 0.43}
		ids := []int64{7, 5, 6, 11, 22, 14, 31, 23, 37}
		tops := []int64{9}
		groupFieldValue := []string{"aaa", "bbb", "ccc", "bbb", "bbb", "ccc", "aaa", "ccc", "aaa"}
		groupByVals := getFieldData("string", groupByField, schemapb.DataType_VarChar, groupFieldValue, 1)
		result1 := &schemapb.SearchResultData{
			Scores: scores,
			TopK:   topK,
			Ids: &schemapb.IDs{
				IdField: &schemapb.IDs_IntId{
					IntId: &schemapb.LongArray{
						Data: ids,
					},
				},
			},
			NumQueries:        nq,
			Topks:             tops,
			GroupByFieldValue: groupByVals,
		}
		subSearchResultData = append(subSearchResultData, result1)
	}
	groupSize := int64(3)

	reducedRes, err := reduceSearchResult(context.Background(), subSearchResultData,
		reduce.NewReduceSearchResultInfo(nq, topK).WithMetricType(metric.L2).WithPkType(schemapb.DataType_Int64).WithGroupByField(groupByField).WithGroupSize(groupSize).WithAdvance(true))

	assert.NoError(t, err)
	// reduce_advance_groupby will only merge results from different delegator without reducing any result
	assert.Equal(t, 9, len(reducedRes.GetResults().Ids.GetIntId().Data))
	assert.Equal(t, 9, len(reducedRes.GetResults().GetScores()))
	assert.Equal(t, 9, len(reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data))
	assert.Equal(t, topK, reducedRes.GetResults().GetTopK())
	assert.Equal(t, []int64{9}, reducedRes.GetResults().GetTopks())

	assert.Equal(t, []int64{7, 5, 6, 11, 22, 14, 31, 23, 37}, reducedRes.GetResults().Ids.GetIntId().Data)
	assert.Equal(t, []float32{0.9, 0.7, 0.65, 0.55, 0.52, 0.51, 0.5, 0.45, 0.43}, reducedRes.GetResults().GetScores())
	assert.Equal(t, []string{"aaa", "bbb", "ccc", "bbb", "bbb", "ccc", "aaa", "ccc", "aaa"}, reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data)
}

func TestTaskSearch_reduceAdvanceSearchGroupByMultipleNq(t *testing.T) {
	groupByField := int64(101)
	nq := int64(2)
	subSearchResultData := make([]*schemapb.SearchResultData, 0)
	topK := int64(2)
	groupSize := int64(2)
	{
		scores := []float32{0.9, 0.7, 0.65, 0.55, 0.51, 0.5, 0.45, 0.43}
		ids := []int64{7, 5, 6, 11, 14, 31, 23, 37}
		tops := []int64{4, 4}
		groupFieldValue := []string{"ccc", "bbb", "ccc", "bbb", "aaa", "xxx", "xxx", "aaa"}
		groupByVals := getFieldData("string", groupByField, schemapb.DataType_VarChar, groupFieldValue, 1)
		result1 := &schemapb.SearchResultData{
			Scores: scores,
			TopK:   topK,
			Ids: &schemapb.IDs{
				IdField: &schemapb.IDs_IntId{
					IntId: &schemapb.LongArray{
						Data: ids,
					},
				},
			},
			NumQueries:        nq,
			Topks:             tops,
			GroupByFieldValue: groupByVals,
		}
		subSearchResultData = append(subSearchResultData, result1)
	}
	{
		scores := []float32{0.83, 0.72, 0.72, 0.65, 0.63, 0.55, 0.52, 0.51}
		ids := []int64{17, 15, 16, 21, 32, 24, 41, 33}
		tops := []int64{4, 4}
		groupFieldValue := []string{"ddd", "bbb", "ddd", "bbb", "rrr", "sss", "rrr", "sss"}
		groupByVals := getFieldData("string", groupByField, schemapb.DataType_VarChar, groupFieldValue, 1)
		result2 := &schemapb.SearchResultData{
			TopK:   topK,
			Scores: scores,
			Ids: &schemapb.IDs{
				IdField: &schemapb.IDs_IntId{
					IntId: &schemapb.LongArray{
						Data: ids,
					},
				},
			},
			Topks:             tops,
			NumQueries:        nq,
			GroupByFieldValue: groupByVals,
		}
		subSearchResultData = append(subSearchResultData, result2)
	}

	reducedRes, err := reduceSearchResult(context.Background(), subSearchResultData,
		reduce.NewReduceSearchResultInfo(nq, topK).WithMetricType(metric.IP).WithPkType(schemapb.DataType_Int64).WithGroupByField(groupByField).WithGroupSize(groupSize).WithAdvance(true))
	assert.NoError(t, err)
	// reduce_advance_groupby will only merge results from different delegator without reducing any result
	assert.Equal(t, 16, len(reducedRes.GetResults().Ids.GetIntId().Data))
	assert.Equal(t, 16, len(reducedRes.GetResults().GetScores()))
	assert.Equal(t, 16, len(reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data))

	assert.Equal(t, topK, reducedRes.GetResults().GetTopK())
	assert.Equal(t, []int64{8, 8}, reducedRes.GetResults().GetTopks())

	assert.Equal(t, []int64{7, 5, 6, 11, 17, 15, 16, 21, 14, 31, 23, 37, 32, 24, 41, 33}, reducedRes.GetResults().Ids.GetIntId().Data)
	assert.Equal(t, []float32{0.9, 0.7, 0.65, 0.55, 0.83, 0.72, 0.72, 0.65, 0.51, 0.5, 0.45, 0.43, 0.63, 0.55, 0.52, 0.51}, reducedRes.GetResults().GetScores())
	assert.Equal(t, []string{"ccc", "bbb", "ccc", "bbb", "ddd", "bbb", "ddd", "bbb", "aaa", "xxx", "xxx", "aaa", "rrr", "sss", "rrr", "sss"}, reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data)

	fmt.Println(reducedRes.GetResults().Ids.GetIntId().Data)
	fmt.Println(reducedRes.GetResults().GetScores())
	fmt.Println(reducedRes.GetResults().GetGroupByFieldValue().GetScalars().GetStringData().Data)
}

func TestSearchTask_ErrExecute(t *testing.T) {
	var (
		err error
		ctx = context.TODO()

		rc             = NewMixCoordMock()
		qn             = getQueryNodeClient()
		shardsNum      = int32(2)
		collectionName = t.Name() + funcutil.GenRandomStr()
	)

	mgr := NewMockShardClientManager(t)
	mgr.EXPECT().GetClient(mock.Anything, mock.Anything).Return(qn, nil).Maybe()
	lb := NewLBPolicyImpl(mgr)

	defer rc.Close()

	err = InitMetaCache(ctx, rc, mgr)
	assert.NoError(t, err)

	fieldName2Types := map[string]schemapb.DataType{
		testBoolField:     schemapb.DataType_Bool,
		testInt32Field:    schemapb.DataType_Int32,
		testInt64Field:    schemapb.DataType_Int64,
		testFloatField:    schemapb.DataType_Float,
		testDoubleField:   schemapb.DataType_Double,
		testFloatVecField: schemapb.DataType_FloatVector,
	}
	if enableMultipleVectorFields {
		fieldName2Types[testBinaryVecField] = schemapb.DataType_BinaryVector
	}

	schema := constructCollectionSchemaByDataType(collectionName, fieldName2Types, testInt64Field, false)
	marshaledSchema, err := proto.Marshal(schema)
	assert.NoError(t, err)

	createColT := &createCollectionTask{
		Condition: NewTaskCondition(ctx),
		CreateCollectionRequest: &milvuspb.CreateCollectionRequest{
			CollectionName: collectionName,
			Schema:         marshaledSchema,
			ShardsNum:      shardsNum,
		},
		ctx:      ctx,
		mixCoord: rc,
	}

	require.NoError(t, createColT.OnEnqueue())
	require.NoError(t, createColT.PreExecute(ctx))
	require.NoError(t, createColT.Execute(ctx))
	require.NoError(t, createColT.PostExecute(ctx))

	collectionID, err := globalMetaCache.GetCollectionID(ctx, GetCurDBNameFromContextOrDefault(ctx), collectionName)
	assert.NoError(t, err)

	successStatus := &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}

	rc.GetShardLeadersFunc = func(ctx context.Context, req *querypb.GetShardLeadersRequest, opts ...grpc.CallOption) (*querypb.GetShardLeadersResponse, error) {
		return &querypb.GetShardLeadersResponse{
			Status: successStatus,
			Shards: []*querypb.ShardLeadersList{
				{
					ChannelName: "channel-1",
					NodeIds:     []int64{1, 2, 3},
					NodeAddrs:   []string{"localhost:9000", "localhost:9001", "localhost:9002"},
					Serviceable: []bool{true, true, true},
				},
			},
		}, nil
	}

	rc.ShowLoadCollectionsFunc = func(ctx context.Context, req *querypb.ShowCollectionsRequest, opts ...grpc.CallOption) (*querypb.ShowCollectionsResponse, error) {
		return &querypb.ShowCollectionsResponse{
			Status:              successStatus,
			CollectionIDs:       []int64{collectionID},
			InMemoryPercentages: []int64{100},
		}, nil
	}

	status, err := rc.LoadCollection(ctx, &querypb.LoadCollectionRequest{
		Base: &commonpb.MsgBase{
			MsgType:  commonpb.MsgType_LoadCollection,
			SourceID: paramtable.GetNodeID(),
		},
		CollectionID: collectionID,
	})
	require.NoError(t, err)
	require.Equal(t, commonpb.ErrorCode_Success, status.ErrorCode)

	// test begins
	task := &searchTask{
		Condition: NewTaskCondition(ctx),
		SearchRequest: &internalpb.SearchRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_Retrieve,
				SourceID: paramtable.GetNodeID(),
			},
			CollectionID:   collectionID,
			OutputFieldsId: make([]int64, len(fieldName2Types)),
		},
		ctx: ctx,
		result: &milvuspb.SearchResults{
			Status: merr.Success(),
		},
		request: &milvuspb.SearchRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_Retrieve,
				SourceID: paramtable.GetNodeID(),
			},
			CollectionName: collectionName,
			Nq:             2,
			DslType:        commonpb.DslType_BoolExprV1,
		},
		mixCoord: rc,
		lb:       lb,
	}
	for i := 0; i < len(fieldName2Types); i++ {
		task.SearchRequest.OutputFieldsId[i] = int64(common.StartOfUserFieldID + i)
	}

	assert.NoError(t, task.OnEnqueue())

	task.ctx = ctx
	if enableMultipleVectorFields {
		err = task.PreExecute(ctx)
		assert.Error(t, err)
		assert.Equal(t, err.Error(), "multiple anns_fields exist, please specify a anns_field in search_params")
	} else {
		assert.NoError(t, task.PreExecute(ctx))
	}

	qn.EXPECT().Search(mock.Anything, mock.Anything).Return(nil, errors.New("mock error"))
	assert.Error(t, task.Execute(ctx))

	qn.ExpectedCalls = nil
	qn.EXPECT().GetComponentStates(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	qn.EXPECT().Search(mock.Anything, mock.Anything).Return(&internalpb.SearchResults{
		Status: merr.Status(merr.ErrChannelNotAvailable),
	}, nil)
	err = task.Execute(ctx)
	assert.ErrorIs(t, err, merr.ErrChannelNotAvailable)

	qn.ExpectedCalls = nil
	qn.EXPECT().GetComponentStates(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	qn.EXPECT().Search(mock.Anything, mock.Anything).Return(&internalpb.SearchResults{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}, nil)
	assert.Error(t, task.Execute(ctx))

	qn.ExpectedCalls = nil
	qn.EXPECT().GetComponentStates(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	qn.EXPECT().Search(mock.Anything, mock.Anything).Return(&internalpb.SearchResults{
		Status: merr.Success(),
	}, nil)
	assert.NoError(t, task.Execute(ctx))
}

func TestTaskSearch_parseSearchInfo(t *testing.T) {
	t.Run("parseSearchInfo no error", func(t *testing.T) {
		var targetOffset int64 = 200

		normalParam := getValidSearchParams()

		noMetricTypeParams := getBaseSearchParams()
		noMetricTypeParams = append(noMetricTypeParams, &commonpb.KeyValuePair{
			Key:   ParamsKey,
			Value: `{"nprobe": 10}`,
		})

		noSearchParams := getBaseSearchParams()
		noSearchParams = append(noSearchParams, &commonpb.KeyValuePair{
			Key:   common.MetricTypeKey,
			Value: metric.L2,
		})

		offsetParam := getValidSearchParams()
		offsetParam = append(offsetParam, &commonpb.KeyValuePair{
			Key:   OffsetKey,
			Value: strconv.FormatInt(targetOffset, 10),
		})

		tests := []struct {
			description string
			validParams []*commonpb.KeyValuePair
		}{
			{"noMetricType", noMetricTypeParams},
			{"noSearchParams", noSearchParams},
			{"normal", normalParam},
			{"offsetParam", offsetParam},
		}

		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				searchInfo, err := parseSearchInfo(test.validParams, nil, nil)
				assert.NoError(t, err)
				assert.NotNil(t, searchInfo.planInfo)
				if test.description == "offsetParam" {
					assert.Equal(t, targetOffset, searchInfo.offset)
				}
			})
		}
	})

	t.Run("parseSearchInfo externalLimit", func(t *testing.T) {
		var externalLimit int64 = 200
		offsetParam := getValidSearchParams()
		offsetParam = append(offsetParam, &commonpb.KeyValuePair{
			Key:   OffsetKey,
			Value: strconv.FormatInt(10, 10),
		})
		rank := &rankParams{
			limit: externalLimit,
		}

		searchInfo, err := parseSearchInfo(offsetParam, nil, rank)
		assert.NoError(t, err)
		assert.NotNil(t, searchInfo.planInfo)
		assert.Equal(t, int64(10), searchInfo.planInfo.GetTopk())
		assert.Equal(t, int64(0), searchInfo.offset)
	})

	t.Run("parseSearchInfo groupBy info for hybrid search", func(t *testing.T) {
		schema := &schemapb.CollectionSchema{
			Fields: []*schemapb.FieldSchema{
				{FieldID: 101, Name: "c1"},
				{FieldID: 102, Name: "c2"},
				{FieldID: 103, Name: "c3"},
			},
		}
		// 1. first parse rank params
		// outer params require to group by field 101 and groupSize=3 and strictGroupSize=false
		testRankParamsPairs := getValidSearchParams()
		testRankParamsPairs = append(testRankParamsPairs, &commonpb.KeyValuePair{
			Key:   GroupByFieldKey,
			Value: "c1",
		})
		testRankParamsPairs = append(testRankParamsPairs, &commonpb.KeyValuePair{
			Key:   GroupSizeKey,
			Value: strconv.FormatInt(3, 10),
		})
		testRankParamsPairs = append(testRankParamsPairs, &commonpb.KeyValuePair{
			Key:   StrictGroupSize,
			Value: "false",
		})
		testRankParamsPairs = append(testRankParamsPairs, &commonpb.KeyValuePair{
			Key:   LimitKey,
			Value: "100",
		})
		testRankParams, err := parseRankParams(testRankParamsPairs, schema)
		assert.NoError(t, err)

		// 2. parse search params for sub request in hybridsearch
		params := getValidSearchParams()
		// inner params require to group by field 103 and groupSize=10 and strictGroupSize=true
		params = append(params, &commonpb.KeyValuePair{
			Key:   GroupByFieldKey,
			Value: "c3",
		})
		params = append(params, &commonpb.KeyValuePair{
			Key:   GroupSizeKey,
			Value: strconv.FormatInt(10, 10),
		})
		params = append(params, &commonpb.KeyValuePair{
			Key:   StrictGroupSize,
			Value: "true",
		})

		searchInfo, err := parseSearchInfo(params, schema, testRankParams)
		assert.NoError(t, err)
		assert.NotNil(t, searchInfo.planInfo)

		// all group_by related parameters should be aligned to parameters
		// set by main request rather than inner sub request
		assert.Equal(t, int64(101), searchInfo.planInfo.GetGroupByFieldId())
		assert.Equal(t, int64(3), searchInfo.planInfo.GetGroupSize())
		assert.False(t, searchInfo.planInfo.GetStrictGroupSize())
	})

	t.Run("parseSearchInfo error", func(t *testing.T) {
		spNoTopk := []*commonpb.KeyValuePair{{
			Key:   AnnsFieldKey,
			Value: testFloatVecField,
		}}

		spInvalidTopk := append(spNoTopk, &commonpb.KeyValuePair{
			Key:   TopKKey,
			Value: "invalid",
		})

		spInvalidTopk65536 := append(spNoTopk, &commonpb.KeyValuePair{
			Key:   TopKKey,
			Value: "65536",
		})

		spNoMetricType := append(spNoTopk, &commonpb.KeyValuePair{
			Key:   TopKKey,
			Value: "10",
		})

		spInvalidTopkPlusOffset := append(spNoTopk, &commonpb.KeyValuePair{
			Key:   OffsetKey,
			Value: "65535",
		})

		spNoSearchParams := append(spNoMetricType, &commonpb.KeyValuePair{
			Key:   common.MetricTypeKey,
			Value: metric.L2,
		})

		// no roundDecimal is valid
		noRoundDecimal := append(spNoSearchParams, &commonpb.KeyValuePair{
			Key:   ParamsKey,
			Value: `{"nprobe": 10}`,
		})

		spInvalidRoundDecimal2 := append(noRoundDecimal, &commonpb.KeyValuePair{
			Key:   RoundDecimalKey,
			Value: "1000",
		})

		spInvalidRoundDecimal := append(noRoundDecimal, &commonpb.KeyValuePair{
			Key:   RoundDecimalKey,
			Value: "invalid",
		})

		spInvalidOffsetNoInt := append(noRoundDecimal, &commonpb.KeyValuePair{
			Key:   OffsetKey,
			Value: "invalid",
		})

		spInvalidOffsetNegative := append(noRoundDecimal, &commonpb.KeyValuePair{
			Key:   OffsetKey,
			Value: "-1",
		})

		spInvalidOffsetTooLarge := append(noRoundDecimal, &commonpb.KeyValuePair{
			Key:   OffsetKey,
			Value: "16386",
		})

		tests := []struct {
			description   string
			invalidParams []*commonpb.KeyValuePair
		}{
			{"No_topk", spNoTopk},
			{"Invalid_topk", spInvalidTopk},
			{"Invalid_topk_65536", spInvalidTopk65536},
			{"Invalid_topk_plus_offset", spInvalidTopkPlusOffset},
			{"Invalid_round_decimal", spInvalidRoundDecimal},
			{"Invalid_round_decimal_1000", spInvalidRoundDecimal2},
			{"Invalid_offset_not_int", spInvalidOffsetNoInt},
			{"Invalid_offset_negative", spInvalidOffsetNegative},
			{"Invalid_offset_too_large", spInvalidOffsetTooLarge},
		}

		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				searchInfo, err := parseSearchInfo(test.invalidParams, nil, nil)
				assert.Error(t, err)
				assert.Nil(t, searchInfo)

				t.Logf("err=%s", err)
			})
		}
	})
	t.Run("check iterator and groupBy", func(t *testing.T) {
		normalParam := getValidSearchParams()
		normalParam = append(normalParam, &commonpb.KeyValuePair{
			Key:   IteratorField,
			Value: "True",
		})
		normalParam = append(normalParam, &commonpb.KeyValuePair{
			Key:   GroupByFieldKey,
			Value: "string_field",
		})
		fields := make([]*schemapb.FieldSchema, 0)
		fields = append(fields, &schemapb.FieldSchema{
			FieldID: int64(101),
			Name:    "string_field",
		})
		schema := &schemapb.CollectionSchema{
			Fields: fields,
		}
		searchInfo, err := parseSearchInfo(normalParam, schema, nil)
		assert.Nil(t, searchInfo)
		assert.ErrorIs(t, err, merr.ErrParameterInvalid)
	})
	t.Run("check range-search and groupBy", func(t *testing.T) {
		normalParam := getValidSearchParams()
		resetSearchParamsValue(normalParam, ParamsKey, `{"nprobe": 10, "radius":0.2}`)
		normalParam = append(normalParam, &commonpb.KeyValuePair{
			Key:   GroupByFieldKey,
			Value: "string_field",
		})
		fields := make([]*schemapb.FieldSchema, 0)
		fields = append(fields, &schemapb.FieldSchema{
			FieldID: int64(101),
			Name:    "string_field",
		})
		schema := &schemapb.CollectionSchema{
			Fields: fields,
		}
		searchInfo, err := parseSearchInfo(normalParam, schema, nil)
		assert.Nil(t, searchInfo)
		assert.ErrorIs(t, err, merr.ErrParameterInvalid)
	})
	t.Run("check nullable and groupBy", func(t *testing.T) {
		normalParam := getValidSearchParams()
		normalParam = append(normalParam, &commonpb.KeyValuePair{
			Key:   GroupByFieldKey,
			Value: "string_field",
		})
		fields := make([]*schemapb.FieldSchema, 0)
		fields = append(fields, &schemapb.FieldSchema{
			FieldID:  int64(101),
			Name:     "string_field",
			Nullable: true,
		})
		schema := &schemapb.CollectionSchema{
			Fields: fields,
		}
		searchInfo, err := parseSearchInfo(normalParam, schema, nil)
		assert.NotNil(t, searchInfo)
		assert.NoError(t, err)
	})
	t.Run("check iterator and topK", func(t *testing.T) {
		normalParam := getValidSearchParams()
		normalParam = append(normalParam, &commonpb.KeyValuePair{
			Key:   IteratorField,
			Value: "True",
		})
		resetSearchParamsValue(normalParam, TopKKey, `1024000`)
		fields := make([]*schemapb.FieldSchema, 0)
		fields = append(fields, &schemapb.FieldSchema{
			FieldID: int64(101),
			Name:    "string_field",
		})
		schema := &schemapb.CollectionSchema{
			Fields: fields,
		}
		searchInfo, err := parseSearchInfo(normalParam, schema, nil)
		assert.NotNil(t, searchInfo)
		assert.NoError(t, err)
		assert.Equal(t, Params.QuotaConfig.TopKLimit.GetAsInt64(), searchInfo.planInfo.GetTopk())
	})

	t.Run("check correctness of group size", func(t *testing.T) {
		normalParam := getValidSearchParams()
		normalParam = append(normalParam, &commonpb.KeyValuePair{
			Key:   GroupSizeKey,
			Value: "128",
		})
		fields := make([]*schemapb.FieldSchema, 0)
		fields = append(fields, &schemapb.FieldSchema{
			FieldID: int64(101),
			Name:    "string_field",
		})
		schema := &schemapb.CollectionSchema{
			Fields: fields,
		}
		_, err := parseSearchInfo(normalParam, schema, nil)
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "exceeds configured max group size"))
		{
			resetSearchParamsValue(normalParam, GroupSizeKey, `10`)
			searchInfo, err := parseSearchInfo(normalParam, schema, nil)
			assert.NoError(t, err)
			assert.Equal(t, int64(10), searchInfo.planInfo.GroupSize)
		}
		{
			resetSearchParamsValue(normalParam, GroupSizeKey, `-1`)
			_, err := parseSearchInfo(normalParam, schema, nil)
			assert.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "is negative"))
		}
		{
			resetSearchParamsValue(normalParam, GroupSizeKey, `xxx`)
			_, err := parseSearchInfo(normalParam, schema, nil)
			assert.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "failed to parse input group size"))
		}
	})

	t.Run("check search iterator v2", func(t *testing.T) {
		kBatchSize := uint32(10)
		generateValidParamsForSearchIteratorV2 := func() []*commonpb.KeyValuePair {
			param := getValidSearchParams()
			return append(param,
				&commonpb.KeyValuePair{
					Key:   SearchIterV2Key,
					Value: "True",
				},
				&commonpb.KeyValuePair{
					Key:   IteratorField,
					Value: "True",
				},
				&commonpb.KeyValuePair{
					Key:   SearchIterBatchSizeKey,
					Value: fmt.Sprintf("%d", kBatchSize),
				},
			)
		}

		t.Run("iteratorV2 normal", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			searchInfo, err := parseSearchInfo(param, nil, nil)
			assert.NoError(t, err)
			assert.NotNil(t, searchInfo.planInfo)
			assert.NotEmpty(t, searchInfo.planInfo.SearchIteratorV2Info.Token)
			assert.Equal(t, kBatchSize, searchInfo.planInfo.SearchIteratorV2Info.BatchSize)
			assert.Len(t, searchInfo.planInfo.SearchIteratorV2Info.Token, 36)
			assert.Equal(t, int64(kBatchSize), searchInfo.planInfo.GetTopk()) // compatibility
		})

		t.Run("iteratorV2 without isIterator", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			resetSearchParamsValue(param, IteratorField, "False")
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "both")
		})

		t.Run("iteratorV2 with groupBy", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			param = append(param, &commonpb.KeyValuePair{
				Key:   GroupByFieldKey,
				Value: "string_field",
			})
			fields := make([]*schemapb.FieldSchema, 0)
			fields = append(fields, &schemapb.FieldSchema{
				FieldID: int64(101),
				Name:    "string_field",
			})
			schema := &schemapb.CollectionSchema{
				Fields: fields,
			}
			_, err := parseSearchInfo(param, schema, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "roupBy")
		})

		t.Run("iteratorV2 with offset", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			param = append(param, &commonpb.KeyValuePair{
				Key:   OffsetKey,
				Value: "10",
			})
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "offset")
		})

		t.Run("iteratorV2 invalid token", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			param = append(param, &commonpb.KeyValuePair{
				Key:   SearchIterIdKey,
				Value: "invalid_token",
			})
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "invalid token format")
		})

		t.Run("iteratorV2 passed token must be same", func(t *testing.T) {
			token, err := uuid.NewRandom()
			assert.NoError(t, err)
			param := generateValidParamsForSearchIteratorV2()
			param = append(param, &commonpb.KeyValuePair{
				Key:   SearchIterIdKey,
				Value: token.String(),
			})
			searchInfo, err := parseSearchInfo(param, nil, nil)
			assert.NoError(t, err)
			assert.NotEmpty(t, searchInfo.planInfo.SearchIteratorV2Info.Token)
			assert.Equal(t, token.String(), searchInfo.planInfo.SearchIteratorV2Info.Token)
		})

		t.Run("iteratorV2 batch size", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			resetSearchParamsValue(param, SearchIterBatchSizeKey, "1.123")
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "batch size is invalid")
		})

		t.Run("iteratorV2 batch size", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			resetSearchParamsValue(param, SearchIterBatchSizeKey, "")
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "batch size is required")
		})

		t.Run("iteratorV2 batch size negative", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			resetSearchParamsValue(param, SearchIterBatchSizeKey, "-1")
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "batch size is invalid")
		})

		t.Run("iteratorV2 batch size too large", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			resetSearchParamsValue(param, SearchIterBatchSizeKey, fmt.Sprintf("%d", Params.QuotaConfig.TopKLimit.GetAsInt64()+1))
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "batch size is invalid")
		})

		t.Run("iteratorV2 last bound", func(t *testing.T) {
			kLastBound := float32(1.123)
			param := generateValidParamsForSearchIteratorV2()
			param = append(param, &commonpb.KeyValuePair{
				Key:   SearchIterLastBoundKey,
				Value: fmt.Sprintf("%f", kLastBound),
			})
			searchInfo, err := parseSearchInfo(param, nil, nil)
			assert.NoError(t, err)
			assert.NotNil(t, searchInfo.planInfo)
			assert.Equal(t, kLastBound, *searchInfo.planInfo.SearchIteratorV2Info.LastBound)
		})

		t.Run("iteratorV2 invalid last bound", func(t *testing.T) {
			param := generateValidParamsForSearchIteratorV2()
			param = append(param, &commonpb.KeyValuePair{
				Key:   SearchIterLastBoundKey,
				Value: "xxx",
			})
			_, err := parseSearchInfo(param, nil, nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "failed to parse input last bound")
		})
	})
}

func getSearchResultData(nq, topk int64) *schemapb.SearchResultData {
	result := schemapb.SearchResultData{
		NumQueries: nq,
		TopK:       topk,
		Ids:        &schemapb.IDs{},
		Scores:     []float32{},
		Topks:      []int64{},
	}
	return &result
}

func TestSearchTask_Requery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		dim        = 128
		rows       = 5
		collection = "test-requery"

		pkField  = "pk"
		vecField = "vec"
	)

	ids := make([]int64, rows)
	for i := range ids {
		ids[i] = int64(i)
	}

	factory := dependency.NewDefaultFactory(true)
	node, err := NewProxy(ctx, factory)
	assert.NoError(t, err)
	node.UpdateStateCode(commonpb.StateCode_Healthy)
	node.tsoAllocator = &timestampAllocator{
		tso: newMockTimestampAllocatorInterface(),
	}
	scheduler, err := newTaskScheduler(ctx, node.tsoAllocator, factory)
	assert.NoError(t, err)
	node.sched = scheduler
	err = node.sched.Start()
	assert.NoError(t, err)
	err = node.initRateCollector()
	assert.NoError(t, err)
	node.mixCoord = mocks.NewMockMixCoordClient(t)

	collectionName := "col"
	collectionID := UniqueID(0)
	cache := NewMockCache(t)
	collSchema := constructCollectionSchema(pkField, vecField, dim, collection)
	schema := newSchemaInfo(collSchema)
	cache.EXPECT().GetCollectionID(mock.Anything, mock.Anything, mock.Anything).Return(collectionID, nil).Maybe()
	cache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schema, nil).Maybe()
	cache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).Return(map[string]int64{"_default": UniqueID(1)}, nil).Maybe()
	cache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(&collectionInfo{}, nil).Maybe()
	cache.EXPECT().GetShard(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]nodeInfo{}, nil).Maybe()
	cache.EXPECT().DeprecateShardCache(mock.Anything, mock.Anything).Return().Maybe()
	globalMetaCache = cache

	t.Run("Test normal", func(t *testing.T) {
		collSchema := constructCollectionSchema(pkField, vecField, dim, collection)
		schema := newSchemaInfo(collSchema)
		qn := mocks.NewMockQueryNodeClient(t)
		qn.EXPECT().Query(mock.Anything, mock.Anything).RunAndReturn(
			func(ctx context.Context, request *querypb.QueryRequest, option ...grpc.CallOption) (*internalpb.RetrieveResults, error) {
				idFieldData := &schemapb.FieldData{
					Type:      schemapb.DataType_Int64,
					FieldName: pkField,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: ids,
								},
							},
						},
					},
				}
				idField := &schemapb.IDs{
					IdField: &schemapb.IDs_IntId{
						IntId: &schemapb.LongArray{
							Data: ids,
						},
					},
				}
				if request.GetReq().GetOutputFieldsId()[0] == 100 {
					return &internalpb.RetrieveResults{
						Ids: idField,
						FieldsData: []*schemapb.FieldData{
							idFieldData,
							newFloatVectorFieldData(vecField, rows, dim),
						},
					}, nil
				}
				return &internalpb.RetrieveResults{
					Ids: idField,
					FieldsData: []*schemapb.FieldData{
						newFloatVectorFieldData(vecField, rows, dim),
						idFieldData,
					},
				}, nil
			})

		lb := NewMockLBPolicy(t)
		lb.EXPECT().Execute(mock.Anything, mock.Anything).Run(func(ctx context.Context, workload CollectionWorkLoad) {
			err = workload.exec(ctx, 0, qn, "")
			assert.NoError(t, err)
		}).Return(nil)
		lb.EXPECT().UpdateCostMetrics(mock.Anything, mock.Anything).Return()
		node.lbPolicy = lb

		resultIDs := &schemapb.IDs{
			IdField: &schemapb.IDs_IntId{
				IntId: &schemapb.LongArray{
					Data: ids,
				},
			},
		}

		outputFields := []string{pkField, vecField}
		qt := &searchTask{
			ctx: ctx,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:  commonpb.MsgType_Search,
					SourceID: paramtable.GetNodeID(),
				},
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collectionName,
				OutputFields:   outputFields,
			},
			result: &milvuspb.SearchResults{
				Results: &schemapb.SearchResultData{
					Ids: resultIDs,
				},
			},
			schema:                 schema,
			tr:                     timerecord.NewTimeRecorder("search"),
			node:                   node,
			translatedOutputFields: outputFields,
		}
		op, err := newRequeryOperator(qt, nil)
		assert.NoError(t, err)
		queryResult, err := op.(*requeryOperator).requery(ctx, nil, qt.result.Results.Ids, outputFields)
		assert.NoError(t, err)
		assert.Len(t, queryResult.FieldsData, 2)
		for _, field := range qt.result.Results.FieldsData {
			fieldName := field.GetFieldName()
			assert.Contains(t, []string{pkField, vecField}, fieldName)
		}
	})

	t.Run("Test no primary key", func(t *testing.T) {
		collSchema := &schemapb.CollectionSchema{}
		schema := newSchemaInfo(collSchema)

		node := mocks.NewMockProxy(t)

		qt := &searchTask{
			ctx: ctx,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:  commonpb.MsgType_Search,
					SourceID: paramtable.GetNodeID(),
				},
			},
			request: &milvuspb.SearchRequest{},
			schema:  schema,
			tr:      timerecord.NewTimeRecorder("search"),
			node:    node,
		}

		_, err := newRequeryOperator(qt, nil)
		t.Logf("err = %s", err)
		assert.Error(t, err)
	})

	t.Run("Test requery failed", func(t *testing.T) {
		collSchema := constructCollectionSchema(pkField, vecField, dim, collection)
		schema := newSchemaInfo(collSchema)
		qn := mocks.NewMockQueryNodeClient(t)
		qn.EXPECT().Query(mock.Anything, mock.Anything).
			Return(nil, errors.New("mock err 1"))

		lb := NewMockLBPolicy(t)
		lb.EXPECT().Execute(mock.Anything, mock.Anything).Run(func(ctx context.Context, workload CollectionWorkLoad) {
			_ = workload.exec(ctx, 0, qn, "")
		}).Return(errors.New("mock err 1"))
		node.lbPolicy = lb

		qt := &searchTask{
			ctx: ctx,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:  commonpb.MsgType_Search,
					SourceID: paramtable.GetNodeID(),
				},
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collectionName,
			},
			schema: schema,
			tr:     timerecord.NewTimeRecorder("search"),
			node:   node,
		}

		op, err := newRequeryOperator(qt, nil)
		assert.NoError(t, err)
		_, err = op.(*requeryOperator).requery(ctx, nil, &schemapb.IDs{}, []string{})
		t.Logf("err = %s", err)
		assert.Error(t, err)
	})

	t.Run("Test postExecute with requery failed", func(t *testing.T) {
		collSchema := constructCollectionSchema(pkField, vecField, dim, collection)
		schema := newSchemaInfo(collSchema)
		qn := mocks.NewMockQueryNodeClient(t)
		qn.EXPECT().Query(mock.Anything, mock.Anything).
			Return(nil, errors.New("mock err 1"))

		lb := NewMockLBPolicy(t)
		lb.EXPECT().Execute(mock.Anything, mock.Anything).Run(func(ctx context.Context, workload CollectionWorkLoad) {
			_ = workload.exec(ctx, 0, qn, "")
		}).Return(errors.New("mock err 1"))
		node.lbPolicy = lb

		resultIDs := &schemapb.IDs{
			IdField: &schemapb.IDs_IntId{
				IntId: &schemapb.LongArray{
					Data: ids,
				},
			},
		}

		qt := &searchTask{
			ctx: ctx,
			SearchRequest: &internalpb.SearchRequest{
				Base: &commonpb.MsgBase{
					MsgType:  commonpb.MsgType_Search,
					SourceID: paramtable.GetNodeID(),
				},
			},
			request: &milvuspb.SearchRequest{
				CollectionName: collectionName,
			},
			result: &milvuspb.SearchResults{
				Results: &schemapb.SearchResultData{
					Ids: resultIDs,
				},
			},
			needRequery: true,
			schema:      schema,
			resultBuf:   typeutil.NewConcurrentSet[*internalpb.SearchResults](),
			tr:          timerecord.NewTimeRecorder("search"),
			node:        node,
		}
		scores := make([]float32, rows)
		for i := range scores {
			scores[i] = float32(i)
		}
		partialResultData := &schemapb.SearchResultData{
			Ids:    resultIDs,
			Scores: scores,
		}
		bytes, err := proto.Marshal(partialResultData)
		assert.NoError(t, err)
		qt.resultBuf.Insert(&internalpb.SearchResults{
			SlicedBlob: bytes,
		})
		qt.queryInfos = []*planpb.QueryInfo{{
			GroupByFieldId: -1,
		}}
		err = qt.PostExecute(ctx)
		t.Logf("err = %s", err)
		assert.Error(t, err)
	})
}

type GetPartitionIDsSuite struct {
	suite.Suite

	mockMetaCache *MockCache
}

func (s *GetPartitionIDsSuite) SetupTest() {
	s.mockMetaCache = NewMockCache(s.T())
	globalMetaCache = s.mockMetaCache
}

func (s *GetPartitionIDsSuite) TearDownTest() {
	globalMetaCache = nil
	Params.Reset(Params.ProxyCfg.PartitionNameRegexp.Key)
}

func (s *GetPartitionIDsSuite) TestPlainPartitionNames() {
	Params.Save(Params.ProxyCfg.PartitionNameRegexp.Key, "false")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]int64{"partition_1": 100, "partition_2": 200}, nil).Once()

	result, err := getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_1", "partition_2"})

	s.NoError(err)
	s.ElementsMatch([]int64{100, 200}, result)

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]int64{"partition_1": 100}, nil).Once()

	_, err = getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_1", "partition_2"})
	s.Error(err)

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("mocked")).Once()
	_, err = getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_1", "partition_2"})
	s.Error(err)
}

func (s *GetPartitionIDsSuite) TestRegexpPartitionNames() {
	Params.Save(Params.ProxyCfg.PartitionNameRegexp.Key, "true")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]int64{"partition_1": 100, "partition_2": 200}, nil).Once()

	result, err := getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_1", "partition_2"})

	s.NoError(err)
	s.ElementsMatch([]int64{100, 200}, result)

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]int64{"partition_1": 100, "partition_2": 200}, nil).Once()

	result, err = getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_.*"})

	s.NoError(err)
	s.ElementsMatch([]int64{100, 200}, result)

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]int64{"partition_1": 100}, nil).Once()

	_, err = getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_1", "partition_2"})
	s.Error(err)

	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("mocked")).Once()
	_, err = getPartitionIDs(ctx, "default_db", "test_collection", []string{"partition_1", "partition_2"})
	s.Error(err)
}

func TestGetPartitionIDs(t *testing.T) {
	suite.Run(t, new(GetPartitionIDsSuite))
}

func TestSearchTask_CanSkipAllocTimestamp(t *testing.T) {
	dbName := "test_query"
	collName := "test_skip_alloc_timestamp"
	collID := UniqueID(111)
	mockMetaCache := NewMockCache(t)
	globalMetaCache = mockMetaCache

	t.Run("default consistency level", func(t *testing.T) {
		st := &searchTask{
			request: &milvuspb.SearchRequest{
				Base:                  nil,
				DbName:                dbName,
				CollectionName:        collName,
				UseDefaultConsistency: true,
			},
		}
		mockMetaCache.EXPECT().GetCollectionID(mock.Anything, mock.Anything, mock.Anything).Return(collID, nil)
		mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			&collectionInfo{
				collID:           collID,
				consistencyLevel: commonpb.ConsistencyLevel_Eventually,
			}, nil).Once()

		skip := st.CanSkipAllocTimestamp()
		assert.True(t, skip)

		mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			&collectionInfo{
				collID:           collID,
				consistencyLevel: commonpb.ConsistencyLevel_Bounded,
			}, nil).Once()
		skip = st.CanSkipAllocTimestamp()
		assert.True(t, skip)

		mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			&collectionInfo{
				collID:           collID,
				consistencyLevel: commonpb.ConsistencyLevel_Strong,
			}, nil).Once()
		skip = st.CanSkipAllocTimestamp()
		assert.False(t, skip)
	})

	t.Run("request consistency level", func(t *testing.T) {
		mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			&collectionInfo{
				collID:           collID,
				consistencyLevel: commonpb.ConsistencyLevel_Eventually,
			}, nil).Times(3)

		st := &searchTask{
			request: &milvuspb.SearchRequest{
				Base:                  nil,
				DbName:                dbName,
				CollectionName:        collName,
				UseDefaultConsistency: false,
				ConsistencyLevel:      commonpb.ConsistencyLevel_Eventually,
			},
		}

		skip := st.CanSkipAllocTimestamp()
		assert.True(t, skip)

		st.request.ConsistencyLevel = commonpb.ConsistencyLevel_Bounded
		skip = st.CanSkipAllocTimestamp()
		assert.True(t, skip)

		st.request.ConsistencyLevel = commonpb.ConsistencyLevel_Strong
		skip = st.CanSkipAllocTimestamp()
		assert.False(t, skip)
	})

	t.Run("legacy_guarantee_ts", func(t *testing.T) {
		st := &searchTask{
			request: &milvuspb.SearchRequest{
				Base:                  nil,
				DbName:                dbName,
				CollectionName:        collName,
				UseDefaultConsistency: false,
				ConsistencyLevel:      commonpb.ConsistencyLevel_Strong,
			},
		}

		skip := st.CanSkipAllocTimestamp()
		assert.False(t, skip)

		st.request.GuaranteeTimestamp = 1 // eventually
		skip = st.CanSkipAllocTimestamp()
		assert.True(t, skip)

		st.request.GuaranteeTimestamp = 2 // bounded
		skip = st.CanSkipAllocTimestamp()
		assert.True(t, skip)
	})

	t.Run("failed", func(t *testing.T) {
		mockMetaCache.ExpectedCalls = nil
		mockMetaCache.EXPECT().GetCollectionID(mock.Anything, mock.Anything, mock.Anything).Return(collID, nil)
		mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			nil, errors.New("mock error")).Once()

		st := &searchTask{
			request: &milvuspb.SearchRequest{
				Base:                  nil,
				DbName:                dbName,
				CollectionName:        collName,
				UseDefaultConsistency: true,
				ConsistencyLevel:      commonpb.ConsistencyLevel_Eventually,
			},
		}

		skip := st.CanSkipAllocTimestamp()
		assert.False(t, skip)

		mockMetaCache.ExpectedCalls = nil
		mockMetaCache.EXPECT().GetCollectionID(mock.Anything, mock.Anything, mock.Anything).Return(collID, errors.New("mock error"))
		mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			&collectionInfo{
				collID:           collID,
				consistencyLevel: commonpb.ConsistencyLevel_Eventually,
			}, nil)

		skip = st.CanSkipAllocTimestamp()
		assert.False(t, skip)

		st2 := &searchTask{
			request: &milvuspb.SearchRequest{
				Base:                  nil,
				DbName:                dbName,
				CollectionName:        collName,
				UseDefaultConsistency: false,
				ConsistencyLevel:      commonpb.ConsistencyLevel_Eventually,
			},
		}

		skip = st2.CanSkipAllocTimestamp()
		assert.True(t, skip)
	})
}

type MaterializedViewTestSuite struct {
	suite.Suite
	mockMetaCache *MockCache

	ctx             context.Context
	cancelFunc      context.CancelFunc
	dbName          string
	colName         string
	colID           UniqueID
	fieldName2Types map[string]schemapb.DataType
}

func (s *MaterializedViewTestSuite) SetupSuite() {
	s.ctx, s.cancelFunc = context.WithCancel(context.Background())
	s.dbName = "TestMvDbName"
	s.colName = "TestMvColName"
	s.colID = UniqueID(123)
	s.fieldName2Types = map[string]schemapb.DataType{
		testInt64Field:    schemapb.DataType_Int64,
		testVarCharField:  schemapb.DataType_VarChar,
		testFloatVecField: schemapb.DataType_FloatVector,
	}
}

func (s *MaterializedViewTestSuite) TearDownSuite() {
	s.cancelFunc()
}

func (s *MaterializedViewTestSuite) SetupTest() {
	s.mockMetaCache = NewMockCache(s.T())
	s.mockMetaCache.EXPECT().GetCollectionID(mock.Anything, mock.Anything, mock.Anything).Return(s.colID, nil)
	s.mockMetaCache.EXPECT().GetCollectionInfo(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		&collectionInfo{
			collID:                s.colID,
			partitionKeyIsolation: true,
		}, nil)
	globalMetaCache = s.mockMetaCache
}

func (s *MaterializedViewTestSuite) TearDownTest() {
	globalMetaCache = nil
}

func (s *MaterializedViewTestSuite) getSearchTask() *searchTask {
	task := &searchTask{
		ctx:            s.ctx,
		collectionName: s.colName,
		SearchRequest:  &internalpb.SearchRequest{},
		request: &milvuspb.SearchRequest{
			DbName:         dbName,
			CollectionName: s.colName,
			Nq:             1,
			SearchParams:   getBaseSearchParams(),
		},
	}
	s.NoError(task.OnEnqueue())
	return task
}

func (s *MaterializedViewTestSuite) TestMvNotEnabledWithNoPartitionKey() {
	task := s.getSearchTask()
	task.enableMaterializedView = false

	schema := constructCollectionSchemaByDataType(s.colName, s.fieldName2Types, testInt64Field, false)
	schemaInfo := newSchemaInfo(schema)
	s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)

	err := task.PreExecute(s.ctx)
	s.NoError(err)
	s.NotZero(len(task.queryInfos))
	s.Equal(false, task.queryInfos[0].MaterializedViewInvolved)
	s.Equal("", task.queryInfos[0].Hints)
}

func (s *MaterializedViewTestSuite) TestMvNotEnabledWithPartitionKey() {
	task := s.getSearchTask()
	task.enableMaterializedView = false
	task.request.Dsl = testInt64Field + " == 1"
	schema := ConstructCollectionSchemaWithPartitionKey(s.colName, s.fieldName2Types, testInt64Field, testInt64Field, false)
	schemaInfo := newSchemaInfo(schema)
	s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)
	s.mockMetaCache.EXPECT().GetPartitionsIndex(mock.Anything, mock.Anything, mock.Anything).Return([]string{"partition_1", "partition_2"}, nil)
	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).Return(map[string]int64{"partition_1": 1, "partition_2": 2}, nil)

	err := task.PreExecute(s.ctx)
	s.NoError(err)
	s.NotZero(len(task.queryInfos))
	s.Equal(false, task.queryInfos[0].MaterializedViewInvolved)
	s.Equal("", task.queryInfos[0].Hints)
}

func (s *MaterializedViewTestSuite) TestMvEnabledNoPartitionKey() {
	task := s.getSearchTask()
	task.enableMaterializedView = true
	schema := constructCollectionSchemaByDataType(s.colName, s.fieldName2Types, testInt64Field, false)
	schemaInfo := newSchemaInfo(schema)
	s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)

	err := task.PreExecute(s.ctx)
	s.NoError(err)
	s.NotZero(len(task.queryInfos))
	s.Equal(false, task.queryInfos[0].MaterializedViewInvolved)
	s.Equal("", task.queryInfos[0].Hints)
}

func (s *MaterializedViewTestSuite) TestMvEnabledPartitionKeyOnInt64() {
	task := s.getSearchTask()
	task.enableMaterializedView = true
	task.request.Dsl = testInt64Field + " == 1"
	schema := ConstructCollectionSchemaWithPartitionKey(s.colName, s.fieldName2Types, testInt64Field, testInt64Field, false)
	schemaInfo := newSchemaInfo(schema)
	s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)
	s.mockMetaCache.EXPECT().GetPartitionsIndex(mock.Anything, mock.Anything, mock.Anything).Return([]string{"partition_1", "partition_2"}, nil)
	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).Return(map[string]int64{"partition_1": 1, "partition_2": 2}, nil)

	err := task.PreExecute(s.ctx)
	s.NoError(err)
	s.NotZero(len(task.queryInfos))
	s.Equal(true, task.queryInfos[0].MaterializedViewInvolved)
	s.Equal("disable", task.queryInfos[0].Hints)
}

func (s *MaterializedViewTestSuite) TestMvEnabledPartitionKeyOnVarChar() {
	task := s.getSearchTask()
	task.enableMaterializedView = true
	task.request.Dsl = testVarCharField + " == \"a\""
	schema := ConstructCollectionSchemaWithPartitionKey(s.colName, s.fieldName2Types, testInt64Field, testVarCharField, false)
	schemaInfo := newSchemaInfo(schema)
	s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)
	s.mockMetaCache.EXPECT().GetPartitionsIndex(mock.Anything, mock.Anything, mock.Anything).Return([]string{"partition_1", "partition_2"}, nil)
	s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).Return(map[string]int64{"partition_1": 1, "partition_2": 2}, nil)

	err := task.PreExecute(s.ctx)
	s.NoError(err)
	s.NotZero(len(task.queryInfos))
	s.Equal(true, task.queryInfos[0].MaterializedViewInvolved)
	s.Equal("disable", task.queryInfos[0].Hints)
}

func (s *MaterializedViewTestSuite) TestMvEnabledPartitionKeyOnVarCharWithIsolation() {
	isAdanceds := []bool{true, false}
	for _, isAdvanced := range isAdanceds {
		task := s.getSearchTask()
		task.enableMaterializedView = true
		task.request.Dsl = testVarCharField + " == \"a\""
		task.IsAdvanced = isAdvanced
		schema := ConstructCollectionSchemaWithPartitionKey(s.colName, s.fieldName2Types, testInt64Field, testVarCharField, false)
		schemaInfo := newSchemaInfo(schema)
		s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)
		s.mockMetaCache.EXPECT().GetPartitionsIndex(mock.Anything, mock.Anything, mock.Anything).Return([]string{"partition_1", "partition_2"}, nil)
		s.mockMetaCache.EXPECT().GetPartitions(mock.Anything, mock.Anything, mock.Anything).Return(map[string]int64{"partition_1": 1, "partition_2": 2}, nil)
		err := task.PreExecute(s.ctx)
		s.NoError(err)
		s.NotZero(len(task.queryInfos))
		s.Equal(true, task.queryInfos[0].MaterializedViewInvolved)
		s.Equal("disable", task.queryInfos[0].Hints)
	}
}

func (s *MaterializedViewTestSuite) TestMvEnabledPartitionKeyOnVarCharWithIsolationInvalid() {
	isAdanceds := []bool{true, false}
	for _, isAdvanced := range isAdanceds {
		task := s.getSearchTask()
		task.enableMaterializedView = true
		task.IsAdvanced = isAdvanced
		task.request.Dsl = testVarCharField + " in [\"a\", \"b\"]"
		schema := ConstructCollectionSchemaWithPartitionKey(s.colName, s.fieldName2Types, testInt64Field, testVarCharField, false)
		schemaInfo := newSchemaInfo(schema)
		s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)
		s.ErrorContains(task.PreExecute(s.ctx), "partition key isolation does not support IN")
	}
}

func (s *MaterializedViewTestSuite) TestMvEnabledPartitionKeyOnVarCharWithIsolationInvalidOr() {
	isAdanceds := []bool{true, false}
	for _, isAdvanced := range isAdanceds {
		task := s.getSearchTask()
		task.enableMaterializedView = true
		task.IsAdvanced = isAdvanced
		task.request.Dsl = testVarCharField + " == \"a\" || " + testVarCharField + "  == \"b\""
		schema := ConstructCollectionSchemaWithPartitionKey(s.colName, s.fieldName2Types, testInt64Field, testVarCharField, false)
		schemaInfo := newSchemaInfo(schema)
		s.mockMetaCache.EXPECT().GetCollectionSchema(mock.Anything, mock.Anything, mock.Anything).Return(schemaInfo, nil)
		s.ErrorContains(task.PreExecute(s.ctx), "partition key isolation does not support OR")
	}
}

func TestMaterializedView(t *testing.T) {
	suite.Run(t, new(MaterializedViewTestSuite))
}

func genTestSearchResultData(nq int64, topk int64, dType schemapb.DataType, fieldName string, fieldId int64, IsAdvanced bool) *internalpb.SearchResults {
	result := &internalpb.SearchResults{
		Base: &commonpb.MsgBase{
			MsgType:   commonpb.MsgType_SearchResult,
			MsgID:     0,
			Timestamp: 0,
			SourceID:  0,
		},
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
			Reason:    "",
		},
		MetricType:               "COSINE",
		NumQueries:               nq,
		TopK:                     topk,
		SealedSegmentIDsSearched: nil,
		ChannelIDsSearched:       nil,
		GlobalSealedSegmentIDs:   nil,
		SlicedBlob:               nil,
		SlicedNumCount:           1,
		SlicedOffset:             0,
		IsAdvanced:               IsAdvanced,
	}

	tops := make([]int64, nq)
	for i := 0; i < int(nq); i++ {
		tops[i] = topk
	}

	resultData := &schemapb.SearchResultData{
		NumQueries: nq,
		TopK:       topk,
		Scores:     testutils.GenerateFloat32Array(int(nq * topk)),
		Ids: &schemapb.IDs{
			IdField: &schemapb.IDs_IntId{
				IntId: &schemapb.LongArray{
					Data: testutils.GenerateInt64Array(int(nq * topk)),
				},
			},
		},
		Topks: tops,
		FieldsData: []*schemapb.FieldData{
			testutils.GenerateScalarFieldData(dType, fieldName, int(nq*topk)),
			testutils.GenerateScalarFieldData(schemapb.DataType_Int64, testInt64Field, int(nq*topk)),
		},
	}
	resultData.FieldsData[0].FieldId = fieldId
	sliceBlob, _ := proto.Marshal(resultData)
	if !IsAdvanced {
		result.SlicedBlob = sliceBlob
	} else {
		result.SubResults = []*internalpb.SubSearchResults{
			{
				SlicedBlob:     sliceBlob,
				SlicedNumCount: 1,
				SlicedOffset:   0,
				MetricType:     "COSINE",
			},
		}
	}
	return result
}

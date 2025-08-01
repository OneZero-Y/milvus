package rootcoord

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/json"
	"github.com/milvus-io/milvus/internal/metastore"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/pkg/v2/common"
	"github.com/milvus-io/milvus/pkg/v2/kv"
	"github.com/milvus-io/milvus/pkg/v2/log"
	pb "github.com/milvus-io/milvus/pkg/v2/proto/etcdpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/internalpb"
	"github.com/milvus-io/milvus/pkg/v2/util"
	"github.com/milvus-io/milvus/pkg/v2/util/conc"
	"github.com/milvus-io/milvus/pkg/v2/util/crypto"
	"github.com/milvus-io/milvus/pkg/v2/util/etcd"
	"github.com/milvus-io/milvus/pkg/v2/util/funcutil"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// prefix/collection/collection_id 					-> CollectionInfo
// prefix/partitions/collection_id/partition_id		-> PartitionInfo
// prefix/aliases/alias_name						-> AliasInfo
// prefix/fields/collection_id/field_id				-> FieldSchema

type Catalog struct {
	Txn      kv.TxnKV
	Snapshot kv.SnapShotKV

	pool *conc.Pool[any]
}

func NewCatalog(metaKV kv.TxnKV, ss kv.SnapShotKV) metastore.RootCoordCatalog {
	ioPool := conc.NewPool[any](paramtable.Get().MetaStoreCfg.ReadConcurrency.GetAsInt())
	return &Catalog{Txn: metaKV, Snapshot: ss, pool: ioPool}
}

func BuildCollectionKey(dbID typeutil.UniqueID, collectionID typeutil.UniqueID) string {
	if dbID != util.NonDBID {
		return BuildCollectionKeyWithDBID(dbID, collectionID)
	}
	return fmt.Sprintf("%s/%d", CollectionMetaPrefix, collectionID)
}

func BuildPartitionPrefix(collectionID typeutil.UniqueID) string {
	return fmt.Sprintf("%s/%d", PartitionMetaPrefix, collectionID)
}

func BuildPartitionKey(collectionID, partitionID typeutil.UniqueID) string {
	return fmt.Sprintf("%s/%d", BuildPartitionPrefix(collectionID), partitionID)
}

func BuildFieldPrefix(collectionID typeutil.UniqueID) string {
	return fmt.Sprintf("%s/%d", FieldMetaPrefix, collectionID)
}

func BuildFieldKey(collectionID typeutil.UniqueID, fieldID int64) string {
	return fmt.Sprintf("%s/%d", BuildFieldPrefix(collectionID), fieldID)
}

func BuildFunctionPrefix(collectionID typeutil.UniqueID) string {
	return fmt.Sprintf("%s/%d", FunctionMetaPrefix, collectionID)
}

func BuildFunctionKey(collectionID typeutil.UniqueID, functionID int64) string {
	return fmt.Sprintf("%s/%d", BuildFunctionPrefix(collectionID), functionID)
}

func BuildStructArrayFieldPrefix(collectionID typeutil.UniqueID) string {
	return fmt.Sprintf("%s/%d", StructArrayFieldMetaPrefix, collectionID)
}

func BuildStructArrayFieldKey(collectionId typeutil.UniqueID, fieldId int64) string {
	return fmt.Sprintf("%s/%d", BuildStructArrayFieldPrefix(collectionId), fieldId)
}

func BuildAliasKey210(alias string) string {
	return fmt.Sprintf("%s/%s", CollectionAliasMetaPrefix210, alias)
}

func BuildAliasKey(aliasName string) string {
	return fmt.Sprintf("%s/%s", AliasMetaPrefix, aliasName)
}

func BuildAliasKeyWithDB(dbID int64, aliasName string) string {
	k := BuildAliasKey(aliasName)
	if dbID == util.NonDBID {
		return k
	}
	return fmt.Sprintf("%s/%s/%d/%s", DatabaseMetaPrefix, Aliases, dbID, aliasName)
}

func BuildAliasPrefixWithDB(dbID int64) string {
	if dbID == util.NonDBID {
		return AliasMetaPrefix
	}
	return fmt.Sprintf("%s/%s/%d", DatabaseMetaPrefix, Aliases, dbID)
}

// since SnapshotKV may save both snapshot key and the original key if the original key is newest
// MaxEtcdTxnNum need to divided by 2
func batchMultiSaveAndRemove(ctx context.Context, snapshot kv.SnapShotKV, limit int, saves map[string]string, removals []string, ts typeutil.Timestamp) error {
	saveFn := func(partialKvs map[string]string) error {
		return snapshot.MultiSave(ctx, partialKvs, ts)
	}
	if err := etcd.SaveByBatchWithLimit(saves, limit, saveFn); err != nil {
		return err
	}

	removeFn := func(partialKeys []string) error {
		return snapshot.MultiSaveAndRemove(ctx, nil, partialKeys, ts)
	}
	return etcd.RemoveByBatchWithLimit(removals, limit, removeFn)
}

func (kc *Catalog) CreateDatabase(ctx context.Context, db *model.Database, ts typeutil.Timestamp) error {
	key := BuildDatabaseKey(db.ID)
	dbInfo := model.MarshalDatabaseModel(db)
	v, err := proto.Marshal(dbInfo)
	if err != nil {
		return err
	}
	return kc.Snapshot.Save(ctx, key, string(v), ts)
}

func (kc *Catalog) AlterDatabase(ctx context.Context, newColl *model.Database, ts typeutil.Timestamp) error {
	key := BuildDatabaseKey(newColl.ID)
	dbInfo := model.MarshalDatabaseModel(newColl)
	v, err := proto.Marshal(dbInfo)
	if err != nil {
		return err
	}
	return kc.Snapshot.Save(ctx, key, string(v), ts)
}

func (kc *Catalog) DropDatabase(ctx context.Context, dbID int64, ts typeutil.Timestamp) error {
	key := BuildDatabaseKey(dbID)
	return kc.Snapshot.MultiSaveAndRemove(ctx, nil, []string{key}, ts)
}

func (kc *Catalog) ListDatabases(ctx context.Context, ts typeutil.Timestamp) ([]*model.Database, error) {
	_, vals, err := kc.Snapshot.LoadWithPrefix(ctx, DBInfoMetaPrefix, ts)
	if err != nil {
		return nil, err
	}

	dbs := make([]*model.Database, 0, len(vals))
	for _, val := range vals {
		dbMeta := &pb.DatabaseInfo{}
		err := proto.Unmarshal([]byte(val), dbMeta)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, model.UnmarshalDatabaseModel(dbMeta))
	}
	return dbs, nil
}

func (kc *Catalog) CreateCollection(ctx context.Context, coll *model.Collection, ts typeutil.Timestamp) error {
	if coll.State != pb.CollectionState_CollectionCreating {
		return fmt.Errorf("cannot create collection with state: %s, collection: %s", coll.State.String(), coll.Name)
	}

	k1 := BuildCollectionKey(coll.DBID, coll.CollectionID)
	collInfo := model.MarshalCollectionModel(coll)
	v1, err := proto.Marshal(collInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal collection info: %s", err.Error())
	}

	// Due to the limit of etcd txn number, we must split these kvs into several batches.
	// Save collection key first, and the state of collection is creating.
	// If we save collection key with error, then no garbage will be generated and error will be raised.
	// If we succeeded to save collection but failed to save other related keys, the garbage meta can be removed
	// outside and the collection won't be seen by any others (since it's of creating state).
	// However, if we save other keys first, there is no chance to remove the intermediate meta.
	if err := kc.Snapshot.Save(ctx, k1, string(v1), ts); err != nil {
		return err
	}

	kvs := map[string]string{}

	// save partition info to new path.
	for _, partition := range coll.Partitions {
		k := BuildPartitionKey(coll.CollectionID, partition.PartitionID)
		partitionInfo := model.MarshalPartitionModel(partition)
		v, err := proto.Marshal(partitionInfo)
		if err != nil {
			return err
		}
		kvs[k] = string(v)
	}

	// no default aliases will be created.
	// save fields info to new path.
	for _, field := range coll.Fields {
		k := BuildFieldKey(coll.CollectionID, field.FieldID)
		fieldInfo := model.MarshalFieldModel(field)
		v, err := proto.Marshal(fieldInfo)
		if err != nil {
			return err
		}
		kvs[k] = string(v)
	}

	// save struct array fields to new path
	for _, structArrayField := range coll.StructArrayFields {
		k := BuildStructArrayFieldKey(coll.CollectionID, structArrayField.FieldID)
		structArrayFieldInfo := model.MarshalStructArrayFieldModel(structArrayField)
		v, err := proto.Marshal(structArrayFieldInfo)
		if err != nil {
			return err
		}
		kvs[k] = string(v)
	}

	// save functions info to new path.
	for _, function := range coll.Functions {
		k := BuildFunctionKey(coll.CollectionID, function.ID)
		functionInfo := model.MarshalFunctionModel(function)
		v, err := proto.Marshal(functionInfo)
		if err != nil {
			return err
		}
		kvs[k] = string(v)
	}

	// Though batchSave is not atomic enough, we can promise the atomicity outside.
	// Recovering from failure, if we found collection is creating, we should remove all these related meta.
	// since SnapshotKV may save both snapshot key and the original key if the original key is newest
	// MaxEtcdTxnNum need to divided by 2
	return etcd.SaveByBatchWithLimit(kvs, util.MaxEtcdTxnNum/2, func(partialKvs map[string]string) error {
		return kc.Snapshot.MultiSave(ctx, partialKvs, ts)
	})
}

func (kc *Catalog) loadCollectionFromDb(ctx context.Context, dbID int64, collectionID typeutil.UniqueID, ts typeutil.Timestamp) (*pb.CollectionInfo, error) {
	collKey := BuildCollectionKey(dbID, collectionID)
	collVal, err := kc.Snapshot.Load(ctx, collKey, ts)
	if err != nil {
		return nil, merr.WrapErrCollectionNotFound(collectionID, err.Error())
	}

	collMeta := &pb.CollectionInfo{}
	err = proto.Unmarshal([]byte(collVal), collMeta)
	return collMeta, err
}

func (kc *Catalog) loadCollectionFromDefaultDb(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) (*pb.CollectionInfo, error) {
	if info, err := kc.loadCollectionFromDb(ctx, util.DefaultDBID, collectionID, ts); err == nil {
		return info, nil
	}
	// get collection from older version.
	return kc.loadCollectionFromDb(ctx, util.NonDBID, collectionID, ts)
}

func (kc *Catalog) loadCollection(ctx context.Context, dbID int64, collectionID typeutil.UniqueID, ts typeutil.Timestamp) (*pb.CollectionInfo, error) {
	if isDefaultDB(dbID) {
		info, err := kc.loadCollectionFromDefaultDb(ctx, collectionID, ts)
		if err != nil {
			return nil, err
		}
		kc.fixDefaultDBIDConsistency(ctx, info, ts)
		return info, nil
	}
	return kc.loadCollectionFromDb(ctx, dbID, collectionID, ts)
}

func partitionVersionAfter210(collMeta *pb.CollectionInfo) bool {
	return len(collMeta.GetPartitionIDs()) <= 0 &&
		len(collMeta.GetPartitionNames()) <= 0 &&
		len(collMeta.GetPartitionCreatedTimestamps()) <= 0
}

func partitionExistByID(collMeta *pb.CollectionInfo, partitionID typeutil.UniqueID) bool {
	return funcutil.SliceContain(collMeta.GetPartitionIDs(), partitionID)
}

func partitionExistByName(collMeta *pb.CollectionInfo, partitionName string) bool {
	return funcutil.SliceContain(collMeta.GetPartitionNames(), partitionName)
}

func (kc *Catalog) CreatePartition(ctx context.Context, dbID int64, partition *model.Partition, ts typeutil.Timestamp) error {
	collMeta, err := kc.loadCollection(ctx, dbID, partition.CollectionID, ts)
	if err != nil {
		return err
	}

	if partitionVersionAfter210(collMeta) {
		// save to newly path.
		k := BuildPartitionKey(partition.CollectionID, partition.PartitionID)
		partitionInfo := model.MarshalPartitionModel(partition)
		v, err := proto.Marshal(partitionInfo)
		if err != nil {
			return err
		}
		return kc.Snapshot.Save(ctx, k, string(v), ts)
	}

	if partitionExistByID(collMeta, partition.PartitionID) {
		return fmt.Errorf("partition already exist: %d", partition.PartitionID)
	}

	if partitionExistByName(collMeta, partition.PartitionName) {
		return fmt.Errorf("partition already exist: %s", partition.PartitionName)
	}

	// keep consistent with older version, otherwise it's hard to judge where to find partitions.
	collMeta.PartitionIDs = append(collMeta.PartitionIDs, partition.PartitionID)
	collMeta.PartitionNames = append(collMeta.PartitionNames, partition.PartitionName)
	collMeta.PartitionCreatedTimestamps = append(collMeta.PartitionCreatedTimestamps, partition.PartitionCreatedTimestamp)

	// this partition exists in older version, should be also changed in place.
	k := BuildCollectionKey(util.NonDBID, partition.CollectionID)
	v, err := proto.Marshal(collMeta)
	if err != nil {
		return err
	}
	return kc.Snapshot.Save(ctx, k, string(v), ts)
}

func (kc *Catalog) CreateAlias(ctx context.Context, alias *model.Alias, ts typeutil.Timestamp) error {
	oldKBefore210 := BuildAliasKey210(alias.Name)
	oldKeyWithoutDb := BuildAliasKey(alias.Name)
	k := BuildAliasKeyWithDB(alias.DbID, alias.Name)
	aliasInfo := model.MarshalAliasModel(alias)
	v, err := proto.Marshal(aliasInfo)
	if err != nil {
		return err
	}
	kvs := map[string]string{k: string(v)}
	return kc.Snapshot.MultiSaveAndRemove(ctx, kvs, []string{oldKBefore210, oldKeyWithoutDb}, ts)
}

func (kc *Catalog) CreateCredential(ctx context.Context, credential *model.Credential) error {
	k := fmt.Sprintf("%s/%s", CredentialPrefix, credential.Username)
	v, err := json.Marshal(&internalpb.CredentialInfo{EncryptedPassword: credential.EncryptedPassword})
	if err != nil {
		log.Ctx(ctx).Error("create credential marshal fail", zap.String("key", k), zap.Error(err))
		return err
	}

	err = kc.Txn.Save(ctx, k, string(v))
	if err != nil {
		log.Ctx(ctx).Error("create credential persist meta fail", zap.String("key", k), zap.Error(err))
		return err
	}

	return nil
}

func (kc *Catalog) AlterCredential(ctx context.Context, credential *model.Credential) error {
	return kc.CreateCredential(ctx, credential)
}

func (kc *Catalog) listPartitionsAfter210(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) ([]*model.Partition, error) {
	prefix := BuildPartitionPrefix(collectionID)
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		return nil, err
	}
	partitions := make([]*model.Partition, 0, len(values))
	for _, v := range values {
		partitionMeta := &pb.PartitionInfo{}
		err := proto.Unmarshal([]byte(v), partitionMeta)
		if err != nil {
			return nil, err
		}
		partitions = append(partitions, model.UnmarshalPartitionModel(partitionMeta))
	}
	return partitions, nil
}

func (kc *Catalog) batchListPartitionsAfter210(ctx context.Context, ts typeutil.Timestamp) (map[int64][]*model.Partition, error) {
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, PartitionMetaPrefix, ts)
	if err != nil {
		return nil, err
	}

	ret := make(map[int64][]*model.Partition)
	for i := 0; i < len(values); i++ {
		partitionMeta := &pb.PartitionInfo{}
		err := proto.Unmarshal([]byte(values[i]), partitionMeta)
		if err != nil {
			return nil, err
		}
		collectionID := partitionMeta.GetCollectionId()
		if ret[collectionID] == nil {
			ret[collectionID] = make([]*model.Partition, 0)
		}
		ret[collectionID] = append(ret[collectionID], model.UnmarshalPartitionModel(partitionMeta))
	}
	return ret, nil
}

func fieldVersionAfter210(collMeta *pb.CollectionInfo) bool {
	return len(collMeta.GetSchema().GetFields()) <= 0 && len(collMeta.GetSchema().GetStructArrayFields()) <= 0
}

func (kc *Catalog) listFieldsAfter210(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) ([]*model.Field, error) {
	prefix := BuildFieldPrefix(collectionID)
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		return nil, err
	}
	fields := make([]*model.Field, 0, len(values))
	for _, v := range values {
		partitionMeta := &schemapb.FieldSchema{}
		err := proto.Unmarshal([]byte(v), partitionMeta)
		if err != nil {
			return nil, err
		}
		fields = append(fields, model.UnmarshalFieldModel(partitionMeta))
	}
	return fields, nil
}

func (kc *Catalog) batchListFieldsAfter210(ctx context.Context, ts typeutil.Timestamp) (map[int64][]*model.Field, error) {
	keys, values, err := kc.Snapshot.LoadWithPrefix(ctx, FieldMetaPrefix, ts)
	if err != nil {
		return nil, err
	}

	ret := make(map[int64][]*model.Field)
	for i := 0; i < len(values); i++ {
		fieldMeta := &schemapb.FieldSchema{}
		err := proto.Unmarshal([]byte(values[i]), fieldMeta)
		if err != nil {
			return nil, err
		}

		collectionID, err := strconv.ParseInt(strings.Split(keys[i], "/")[2], 10, 64)
		if err != nil {
			return nil, err
		}
		if ret[collectionID] == nil {
			ret[collectionID] = make([]*model.Field, 0)
		}
		ret[collectionID] = append(ret[collectionID], model.UnmarshalFieldModel(fieldMeta))
	}
	return ret, nil
}

func (kc *Catalog) listStructArrayFieldsAfter210(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) ([]*model.StructArrayField, error) {
	prefix := BuildStructArrayFieldPrefix(collectionID)
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		return nil, err
	}
	structFields := make([]*model.StructArrayField, 0, len(values))
	for _, v := range values {
		partitionMeta := &schemapb.StructArrayFieldSchema{}
		err := proto.Unmarshal([]byte(v), partitionMeta)
		if err != nil {
			return nil, err
		}
		structFields = append(structFields, model.UnmarshalStructArrayFieldModel(partitionMeta))
	}
	return structFields, nil
}

func (kc *Catalog) listFunctions(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) ([]*model.Function, error) {
	prefix := BuildFunctionPrefix(collectionID)
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		return nil, err
	}
	functions := make([]*model.Function, 0, len(values))
	for _, v := range values {
		functionSchema := &schemapb.FunctionSchema{}
		err := proto.Unmarshal([]byte(v), functionSchema)
		if err != nil {
			return nil, err
		}
		functions = append(functions, model.UnmarshalFunctionModel(functionSchema))
	}
	return functions, nil
}

func (kc *Catalog) batchListFunctions(ctx context.Context, ts typeutil.Timestamp) (map[int64][]*model.Function, error) {
	keys, values, err := kc.Snapshot.LoadWithPrefix(ctx, FunctionMetaPrefix, ts)
	if err != nil {
		return nil, err
	}
	ret := make(map[int64][]*model.Function)
	for i := 0; i < len(values); i++ {
		functionSchema := &schemapb.FunctionSchema{}
		err := proto.Unmarshal([]byte(values[i]), functionSchema)
		if err != nil {
			return nil, err
		}
		collectionID, err := strconv.ParseInt(strings.Split(keys[i], "/")[2], 10, 64)
		if err != nil {
			return nil, err
		}
		if ret[collectionID] == nil {
			ret[collectionID] = make([]*model.Function, 0)
		}
		ret[collectionID] = append(ret[collectionID], model.UnmarshalFunctionModel(functionSchema))
	}
	return ret, nil
}

func (kc *Catalog) appendPartitionAndFieldsInfo(ctx context.Context, collMeta *pb.CollectionInfo,
	ts typeutil.Timestamp,
) (*model.Collection, error) {
	collection := model.UnmarshalCollectionModel(collMeta)

	if !partitionVersionAfter210(collMeta) && !fieldVersionAfter210(collMeta) {
		return collection, nil
	}

	partitions, err := kc.listPartitionsAfter210(ctx, collection.CollectionID, ts)
	if err != nil {
		return nil, err
	}
	collection.Partitions = partitions

	fields, err := kc.listFieldsAfter210(ctx, collection.CollectionID, ts)
	if err != nil {
		return nil, err
	}
	collection.Fields = fields

	structArrayFields, err := kc.listStructArrayFieldsAfter210(ctx, collection.CollectionID, ts)
	if err != nil {
		return nil, err
	}
	collection.StructArrayFields = structArrayFields

	functions, err := kc.listFunctions(ctx, collection.CollectionID, ts)
	if err != nil {
		return nil, err
	}
	collection.Functions = functions
	return collection, nil
}

func (kc *Catalog) batchAppendPartitionAndFieldsInfo(ctx context.Context, collMeta []*pb.CollectionInfo,
	ts typeutil.Timestamp,
) ([]*model.Collection, error) {
	var partitionMetaMap map[int64][]*model.Partition
	var fieldMetaMap map[int64][]*model.Field
	var functionMetaMap map[int64][]*model.Function
	ret := make([]*model.Collection, 0)
	for _, coll := range collMeta {
		collection := model.UnmarshalCollectionModel(coll)
		if partitionVersionAfter210(coll) || fieldVersionAfter210(coll) {
			if len(partitionMetaMap) == 0 {
				var err error
				partitionMetaMap, err = kc.batchListPartitionsAfter210(ctx, ts)
				if err != nil {
					return nil, err
				}

				fieldMetaMap, err = kc.batchListFieldsAfter210(ctx, ts)
				if err != nil {
					return nil, err
				}

				functionMetaMap, err = kc.batchListFunctions(ctx, ts)
				if err != nil {
					return nil, err
				}
			}

			if partitionMetaMap[collection.CollectionID] != nil {
				collection.Partitions = partitionMetaMap[collection.CollectionID]
			}
			if fieldMetaMap[collection.CollectionID] != nil {
				collection.Fields = fieldMetaMap[collection.CollectionID]
			}
			if functionMetaMap[collection.CollectionID] != nil {
				collection.Functions = functionMetaMap[collection.CollectionID]
			}
		}
		ret = append(ret, collection)
	}

	return ret, nil
}

func (kc *Catalog) GetCollectionByID(ctx context.Context, dbID int64, ts typeutil.Timestamp, collectionID typeutil.UniqueID) (*model.Collection, error) {
	collMeta, err := kc.loadCollection(ctx, dbID, collectionID, ts)
	if err != nil {
		return nil, err
	}

	return kc.appendPartitionAndFieldsInfo(ctx, collMeta, ts)
}

func (kc *Catalog) CollectionExists(ctx context.Context, dbID int64, collectionID typeutil.UniqueID, ts typeutil.Timestamp) bool {
	_, err := kc.GetCollectionByID(ctx, dbID, ts, collectionID)
	return err == nil
}

func (kc *Catalog) GetCredential(ctx context.Context, username string) (*model.Credential, error) {
	k := fmt.Sprintf("%s/%s", CredentialPrefix, username)
	v, err := kc.Txn.Load(ctx, k)
	if err != nil {
		if errors.Is(err, merr.ErrIoKeyNotFound) {
			log.Ctx(ctx).Debug("not found the user", zap.String("key", k))
		} else {
			log.Ctx(ctx).Warn("get credential meta fail", zap.String("key", k), zap.Error(err))
		}
		return nil, err
	}

	credentialInfo := internalpb.CredentialInfo{}
	err = json.Unmarshal([]byte(v), &credentialInfo)
	if err != nil {
		return nil, fmt.Errorf("unmarshal credential info err:%w", err)
	}

	return &model.Credential{Username: username, EncryptedPassword: credentialInfo.EncryptedPassword}, nil
}

func (kc *Catalog) AlterAlias(ctx context.Context, alias *model.Alias, ts typeutil.Timestamp) error {
	return kc.CreateAlias(ctx, alias, ts)
}

func (kc *Catalog) DropCollection(ctx context.Context, collectionInfo *model.Collection, ts typeutil.Timestamp) error {
	collectionKeys := []string{BuildCollectionKey(collectionInfo.DBID, collectionInfo.CollectionID)}

	var delMetakeysSnap []string
	for _, alias := range collectionInfo.Aliases {
		delMetakeysSnap = append(delMetakeysSnap,
			BuildAliasKey210(alias),
			BuildAliasKey(alias),
			BuildAliasKeyWithDB(collectionInfo.DBID, alias),
		)
	}
	// Snapshot will list all (k, v) pairs and then use Txn.MultiSave to save tombstone for these keys when it prepares
	// to remove a prefix, so though we have very few prefixes, the final operations may exceed the max txn number.
	// TODO(longjiquan): should we list all partitions & fields in KV anyway?
	for _, partition := range collectionInfo.Partitions {
		delMetakeysSnap = append(delMetakeysSnap, BuildPartitionKey(collectionInfo.CollectionID, partition.PartitionID))
	}
	for _, field := range collectionInfo.Fields {
		delMetakeysSnap = append(delMetakeysSnap, BuildFieldKey(collectionInfo.CollectionID, field.FieldID))
	}
	for _, structArrayField := range collectionInfo.StructArrayFields {
		delMetakeysSnap = append(delMetakeysSnap, BuildStructArrayFieldKey(collectionInfo.CollectionID, structArrayField.FieldID))
	}
	for _, function := range collectionInfo.Functions {
		delMetakeysSnap = append(delMetakeysSnap, BuildFunctionKey(collectionInfo.CollectionID, function.ID))
	}
	// delMetakeysSnap = append(delMetakeysSnap, buildPartitionPrefix(collectionInfo.CollectionID))
	// delMetakeysSnap = append(delMetakeysSnap, buildFieldPrefix(collectionInfo.CollectionID))

	// Though batchMultiSaveAndRemoveWithPrefix is not atomic enough, we can promise atomicity outside.
	// If we found collection under dropping state, we'll know that gc is not completely on this collection.
	// However, if we remove collection first, we cannot remove other metas.
	// since SnapshotKV may save both snapshot key and the original key if the original key is newest
	// MaxEtcdTxnNum need to divided by 2
	if err := batchMultiSaveAndRemove(ctx, kc.Snapshot, util.MaxEtcdTxnNum/2, nil, delMetakeysSnap, ts); err != nil {
		return err
	}

	// if we found collection dropping, we should try removing related resources.
	return kc.Snapshot.MultiSaveAndRemove(ctx, nil, collectionKeys, ts)
}

func (kc *Catalog) alterModifyCollection(ctx context.Context, oldColl *model.Collection, newColl *model.Collection, ts typeutil.Timestamp, fieldModify bool) error {
	if oldColl.TenantID != newColl.TenantID || oldColl.CollectionID != newColl.CollectionID {
		return errors.New("altering tenant id or collection id is forbidden")
	}
	if oldColl.DBID != newColl.DBID {
		return errors.New("altering dbID should use `AlterCollectionDB` interface")
	}
	oldCollClone := oldColl.Clone()
	oldCollClone.DBID = newColl.DBID
	oldCollClone.Name = newColl.Name
	oldCollClone.Description = newColl.Description
	oldCollClone.AutoID = newColl.AutoID
	oldCollClone.VirtualChannelNames = newColl.VirtualChannelNames
	oldCollClone.PhysicalChannelNames = newColl.PhysicalChannelNames
	oldCollClone.StartPositions = newColl.StartPositions
	oldCollClone.ShardsNum = newColl.ShardsNum
	oldCollClone.CreateTime = newColl.CreateTime
	oldCollClone.ConsistencyLevel = newColl.ConsistencyLevel
	oldCollClone.State = newColl.State
	oldCollClone.Properties = newColl.Properties
	oldCollClone.Fields = newColl.Fields
	oldCollClone.StructArrayFields = newColl.StructArrayFields
	oldCollClone.UpdateTimestamp = newColl.UpdateTimestamp

	newKey := BuildCollectionKey(newColl.DBID, oldColl.CollectionID)
	value, err := proto.Marshal(model.MarshalCollectionModel(oldCollClone))
	if err != nil {
		return err
	}
	saves := map[string]string{newKey: string(value)}
	// no default aliases will be created.
	// save fields info to new path.
	if fieldModify {
		for _, field := range newColl.Fields {
			k := BuildFieldKey(newColl.CollectionID, field.FieldID)
			fieldInfo := model.MarshalFieldModel(field)
			v, err := proto.Marshal(fieldInfo)
			if err != nil {
				return err
			}
			saves[k] = string(v)
		}

		for _, structArrayField := range newColl.StructArrayFields {
			k := BuildStructArrayFieldKey(newColl.CollectionID, structArrayField.FieldID)
			structArrayFieldInfo := model.MarshalStructArrayFieldModel(structArrayField)
			v, err := proto.Marshal(structArrayFieldInfo)
			if err != nil {
				return err
			}
			saves[k] = string(v)
		}
	}

	return etcd.SaveByBatchWithLimit(saves, util.MaxEtcdTxnNum/2, func(partialKvs map[string]string) error {
		return kc.Snapshot.MultiSave(ctx, partialKvs, ts)
	})
}

func (kc *Catalog) AlterCollection(ctx context.Context, oldColl *model.Collection, newColl *model.Collection, alterType metastore.AlterType, ts typeutil.Timestamp, fieldModify bool) error {
	switch alterType {
	case metastore.MODIFY:
		return kc.alterModifyCollection(ctx, oldColl, newColl, ts, fieldModify)
	default:
		return fmt.Errorf("altering collection doesn't support %s", alterType.String())
	}
}

func (kc *Catalog) AlterCollectionDB(ctx context.Context, oldColl *model.Collection, newColl *model.Collection, ts typeutil.Timestamp) error {
	if oldColl.TenantID != newColl.TenantID || oldColl.CollectionID != newColl.CollectionID {
		return errors.New("altering tenant id or collection id is forbidden")
	}
	oldKey := BuildCollectionKey(oldColl.DBID, oldColl.CollectionID)
	newKey := BuildCollectionKey(newColl.DBID, newColl.CollectionID)

	value, err := proto.Marshal(model.MarshalCollectionModel(newColl))
	if err != nil {
		return err
	}
	saves := map[string]string{newKey: string(value)}

	return kc.Snapshot.MultiSaveAndRemove(ctx, saves, []string{oldKey}, ts)
}

func (kc *Catalog) alterModifyPartition(ctx context.Context, oldPart *model.Partition, newPart *model.Partition, ts typeutil.Timestamp) error {
	if oldPart.CollectionID != newPart.CollectionID || oldPart.PartitionID != newPart.PartitionID {
		return errors.New("altering collection id or partition id is forbidden")
	}
	oldPartClone := oldPart.Clone()
	newPartClone := newPart.Clone()
	oldPartClone.PartitionName = newPartClone.PartitionName
	oldPartClone.PartitionCreatedTimestamp = newPartClone.PartitionCreatedTimestamp
	oldPartClone.State = newPartClone.State
	key := BuildPartitionKey(oldPart.CollectionID, oldPart.PartitionID)
	value, err := proto.Marshal(model.MarshalPartitionModel(oldPartClone))
	if err != nil {
		return err
	}
	return kc.Snapshot.Save(ctx, key, string(value), ts)
}

func (kc *Catalog) AlterPartition(ctx context.Context, dbID int64, oldPart *model.Partition, newPart *model.Partition, alterType metastore.AlterType, ts typeutil.Timestamp) error {
	if alterType == metastore.MODIFY {
		return kc.alterModifyPartition(ctx, oldPart, newPart, ts)
	}
	return fmt.Errorf("altering partition doesn't support %s", alterType.String())
}

func dropPartition(collMeta *pb.CollectionInfo, partitionID typeutil.UniqueID) {
	if collMeta == nil {
		return
	}

	{
		loc := -1
		for idx, pid := range collMeta.GetPartitionIDs() {
			if pid == partitionID {
				loc = idx
				break
			}
		}
		if loc != -1 {
			collMeta.PartitionIDs = append(collMeta.GetPartitionIDs()[:loc], collMeta.GetPartitionIDs()[loc+1:]...)
			collMeta.PartitionNames = append(collMeta.GetPartitionNames()[:loc], collMeta.GetPartitionNames()[loc+1:]...)
			collMeta.PartitionCreatedTimestamps = append(collMeta.GetPartitionCreatedTimestamps()[:loc], collMeta.GetPartitionCreatedTimestamps()[loc+1:]...)
		}
	}
}

func (kc *Catalog) DropPartition(ctx context.Context, dbID int64, collectionID typeutil.UniqueID, partitionID typeutil.UniqueID, ts typeutil.Timestamp) error {
	collMeta, err := kc.loadCollection(ctx, dbID, collectionID, ts)
	if errors.Is(err, merr.ErrCollectionNotFound) {
		// collection's gc happened before partition's.
		return nil
	}

	if err != nil {
		return err
	}

	if partitionVersionAfter210(collMeta) {
		k := BuildPartitionKey(collectionID, partitionID)
		return kc.Snapshot.MultiSaveAndRemove(ctx, nil, []string{k}, ts)
	}

	k := BuildCollectionKey(util.NonDBID, collectionID)
	dropPartition(collMeta, partitionID)
	v, err := proto.Marshal(collMeta)
	if err != nil {
		return err
	}
	return kc.Snapshot.Save(ctx, k, string(v), ts)
}

func (kc *Catalog) DropCredential(ctx context.Context, username string) error {
	k := fmt.Sprintf("%s/%s", CredentialPrefix, username)
	userResults, err := kc.ListUser(ctx, util.DefaultTenant, &milvuspb.UserEntity{Name: username}, true)
	if err != nil && !errors.Is(err, merr.ErrIoKeyNotFound) {
		log.Ctx(ctx).Warn("fail to list user", zap.String("key", k), zap.Error(err))
		return err
	}
	deleteKeys := make([]string, 0, len(userResults)+1)
	deleteKeys = append(deleteKeys, k)
	for _, userResult := range userResults {
		if userResult.User.Name == username {
			for _, role := range userResult.Roles {
				userRoleKey := funcutil.HandleTenantForEtcdKey(RoleMappingPrefix, util.DefaultTenant, fmt.Sprintf("%s/%s", username, role.Name))
				deleteKeys = append(deleteKeys, userRoleKey)
			}
		}
	}
	err = kc.Txn.MultiRemove(ctx, deleteKeys)
	if err != nil {
		log.Ctx(ctx).Warn("fail to drop credential", zap.String("key", k), zap.Error(err))
		return err
	}

	return nil
}

func (kc *Catalog) DropAlias(ctx context.Context, dbID int64, alias string, ts typeutil.Timestamp) error {
	oldKBefore210 := BuildAliasKey210(alias)
	oldKeyWithoutDb := BuildAliasKey(alias)
	k := BuildAliasKeyWithDB(dbID, alias)
	return kc.Snapshot.MultiSaveAndRemove(ctx, nil, []string{k, oldKeyWithoutDb, oldKBefore210}, ts)
}

func (kc *Catalog) GetCollectionByName(ctx context.Context, dbID int64, collectionName string, ts typeutil.Timestamp) (*model.Collection, error) {
	prefix := getDatabasePrefix(dbID)
	_, vals, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		log.Ctx(ctx).Warn("get collection meta fail", zap.String("collectionName", collectionName), zap.Error(err))
		return nil, err
	}

	for _, val := range vals {
		colMeta := pb.CollectionInfo{}
		err = proto.Unmarshal([]byte(val), &colMeta)
		if err != nil {
			log.Ctx(ctx).Warn("get collection meta unmarshal fail", zap.String("collectionName", collectionName), zap.Error(err))
			continue
		}
		if colMeta.Schema.Name == collectionName {
			// compatibility handled by kc.GetCollectionByID.
			return kc.GetCollectionByID(ctx, dbID, ts, colMeta.GetID())
		}
	}

	return nil, merr.WrapErrCollectionNotFoundWithDB(dbID, collectionName, fmt.Sprintf("timestamp = %d", ts))
}

func (kc *Catalog) ListCollections(ctx context.Context, dbID int64, ts typeutil.Timestamp) ([]*model.Collection, error) {
	prefix := getDatabasePrefix(dbID)
	_, vals, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		log.Ctx(ctx).Error("get collections meta fail",
			zap.String("prefix", prefix),
			zap.Uint64("timestamp", ts),
			zap.Error(err))
		return nil, err
	}

	start := time.Now()
	colls := make([]*model.Collection, len(vals))
	futures := make([]*conc.Future[any], 0, len(vals))
	for i, val := range vals {
		i := i
		val := val
		futures = append(futures, kc.pool.Submit(func() (any, error) {
			collMeta := &pb.CollectionInfo{}
			err := proto.Unmarshal([]byte(val), collMeta)
			if err != nil {
				log.Ctx(ctx).Warn("unmarshal collection info failed", zap.Error(err))
				return nil, err
			}
			kc.fixDefaultDBIDConsistency(ctx, collMeta, ts)
			collection, err := kc.appendPartitionAndFieldsInfo(ctx, collMeta, ts)
			if err != nil {
				return nil, err
			}
			colls[i] = collection
			return nil, nil
		}))
	}
	err = conc.AwaitAll(futures...)
	if err != nil {
		return nil, err
	}
	log.Ctx(ctx).Info("unmarshal all collection details cost", zap.Int64("db", dbID), zap.Duration("cost", time.Since(start)))
	return colls, nil
}

// fixDefaultDBIDConsistency fix dbID consistency for collectionInfo.
// We have two versions of default databaseID (0 at legacy path, 1 at new path), we should keep consistent view when user use default database.
// all collections in default database should be marked with dbID 1.
// this method also update dbid in meta store when dbid is 0
// see also: https://github.com/milvus-io/milvus/issues/33608
func (kc *Catalog) fixDefaultDBIDConsistency(ctx context.Context, collMeta *pb.CollectionInfo, ts typeutil.Timestamp) {
	if collMeta.DbId == util.NonDBID {
		coll := model.UnmarshalCollectionModel(collMeta)
		cloned := coll.Clone()
		cloned.DBID = util.DefaultDBID
		kc.AlterCollectionDB(ctx, coll, cloned, ts)

		collMeta.DbId = util.DefaultDBID
	}
}

func (kc *Catalog) listAliasesBefore210(ctx context.Context, ts typeutil.Timestamp) ([]*model.Alias, error) {
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, CollectionAliasMetaPrefix210, ts)
	if err != nil {
		return nil, err
	}
	// aliases before 210 stored by CollectionInfo.
	aliases := make([]*model.Alias, 0, len(values))
	for _, value := range values {
		coll := &pb.CollectionInfo{}
		err := proto.Unmarshal([]byte(value), coll)
		if err != nil {
			return nil, err
		}
		aliases = append(aliases, &model.Alias{
			Name:         coll.GetSchema().GetName(),
			CollectionID: coll.GetID(),
			CreatedTime:  0, // not accurate.
			DbID:         coll.DbId,
		})
	}
	return aliases, nil
}

func (kc *Catalog) listAliasesAfter210WithDb(ctx context.Context, dbID int64, ts typeutil.Timestamp) ([]*model.Alias, error) {
	prefix := BuildAliasPrefixWithDB(dbID)
	_, values, err := kc.Snapshot.LoadWithPrefix(ctx, prefix, ts)
	if err != nil {
		return nil, err
	}
	// aliases after 210 stored by AliasInfo.
	aliases := make([]*model.Alias, 0, len(values))
	for _, value := range values {
		info := &pb.AliasInfo{}
		err := proto.Unmarshal([]byte(value), info)
		if err != nil {
			return nil, err
		}
		aliases = append(aliases, &model.Alias{
			Name:         info.GetAliasName(),
			CollectionID: info.GetCollectionId(),
			CreatedTime:  info.GetCreatedTime(),
			DbID:         dbID,
		})
	}
	return aliases, nil
}

func (kc *Catalog) listAliasesInDefaultDb(ctx context.Context, ts typeutil.Timestamp) ([]*model.Alias, error) {
	aliases1, err := kc.listAliasesBefore210(ctx, ts)
	if err != nil {
		return nil, err
	}
	aliases2, err := kc.listAliasesAfter210WithDb(ctx, util.DefaultDBID, ts)
	if err != nil {
		return nil, err
	}
	aliases3, err := kc.listAliasesAfter210WithDb(ctx, util.NonDBID, ts)
	if err != nil {
		return nil, err
	}
	aliases := append(aliases1, aliases2...)
	aliases = append(aliases, aliases3...)
	return aliases, nil
}

func (kc *Catalog) ListAliases(ctx context.Context, dbID int64, ts typeutil.Timestamp) ([]*model.Alias, error) {
	if !isDefaultDB(dbID) {
		return kc.listAliasesAfter210WithDb(ctx, dbID, ts)
	}
	return kc.listAliasesInDefaultDb(ctx, ts)
}

func (kc *Catalog) ListCredentials(ctx context.Context) ([]string, error) {
	users, err := kc.ListCredentialsWithPasswd(ctx)
	if err != nil {
		return nil, err
	}
	return lo.Keys(users), nil
}

func (kc *Catalog) ListCredentialsWithPasswd(ctx context.Context) (map[string]string, error) {
	keys, values, err := kc.Txn.LoadWithPrefix(ctx, CredentialPrefix)
	if err != nil {
		log.Ctx(ctx).Error("list all credential usernames fail", zap.String("prefix", CredentialPrefix), zap.Error(err))
		return nil, err
	}

	users := make(map[string]string)
	for i := range keys {
		username := typeutil.After(keys[i], UserSubPrefix+"/")
		if len(username) == 0 {
			log.Ctx(ctx).Warn("no username extract from path:", zap.String("path", keys[i]))
			continue
		}
		credential := &internalpb.CredentialInfo{}
		err := json.Unmarshal([]byte(values[i]), credential)
		if err != nil {
			log.Ctx(ctx).Error("credential unmarshal fail", zap.String("key", keys[i]), zap.Error(err))
			return nil, err
		}
		users[username] = credential.EncryptedPassword
	}

	return users, nil
}

func (kc *Catalog) save(ctx context.Context, k string) error {
	var err error
	if _, err = kc.Txn.Load(ctx, k); err != nil && !errors.Is(err, merr.ErrIoKeyNotFound) {
		return err
	}
	if err == nil {
		log.Ctx(ctx).Debug("the key has existed", zap.String("key", k))
		return common.NewIgnorableError(fmt.Errorf("the key[%s] has existed", k))
	}
	return kc.Txn.Save(ctx, k, "")
}

func (kc *Catalog) remove(ctx context.Context, k string) error {
	var err error
	if _, err = kc.Txn.Load(ctx, k); err != nil && !errors.Is(err, merr.ErrIoKeyNotFound) {
		return err
	}
	if err != nil && errors.Is(err, merr.ErrIoKeyNotFound) {
		log.Ctx(ctx).Debug("the key isn't existed", zap.String("key", k))
		return common.NewIgnorableError(fmt.Errorf("the key[%s] isn't existed", k))
	}
	return kc.Txn.Remove(ctx, k)
}

func (kc *Catalog) CreateRole(ctx context.Context, tenant string, entity *milvuspb.RoleEntity) error {
	k := funcutil.HandleTenantForEtcdKey(RolePrefix, tenant, entity.Name)
	err := kc.save(ctx, k)
	if err != nil && !common.IsIgnorableError(err) {
		log.Ctx(ctx).Warn("fail to save the role", zap.String("key", k), zap.Error(err))
	}
	return err
}

func (kc *Catalog) DropRole(ctx context.Context, tenant string, roleName string) error {
	k := funcutil.HandleTenantForEtcdKey(RolePrefix, tenant, roleName)
	roleResults, err := kc.ListRole(ctx, tenant, &milvuspb.RoleEntity{Name: roleName}, true)
	if err != nil && !errors.Is(err, merr.ErrIoKeyNotFound) {
		log.Ctx(ctx).Warn("fail to list role", zap.String("key", k), zap.Error(err))
		return err
	}

	deleteKeys := make([]string, 0, len(roleResults)+1)
	deleteKeys = append(deleteKeys, k)
	for _, roleResult := range roleResults {
		if roleResult.Role.Name == roleName {
			for _, userInfo := range roleResult.Users {
				userRoleKey := funcutil.HandleTenantForEtcdKey(RoleMappingPrefix, tenant, fmt.Sprintf("%s/%s", userInfo.Name, roleName))
				deleteKeys = append(deleteKeys, userRoleKey)
			}
		}
	}

	err = kc.Txn.MultiRemove(ctx, deleteKeys)
	if err != nil {
		log.Ctx(ctx).Warn("fail to drop role", zap.String("key", k), zap.Error(err))
		return err
	}
	return nil
}

func (kc *Catalog) AlterUserRole(ctx context.Context, tenant string, userEntity *milvuspb.UserEntity, roleEntity *milvuspb.RoleEntity, operateType milvuspb.OperateUserRoleType) error {
	k := funcutil.HandleTenantForEtcdKey(RoleMappingPrefix, tenant, fmt.Sprintf("%s/%s", userEntity.Name, roleEntity.Name))
	var err error
	if operateType == milvuspb.OperateUserRoleType_AddUserToRole {
		err = kc.save(ctx, k)
		if err != nil {
			log.Ctx(ctx).Error("fail to save the user-role", zap.String("key", k), zap.Error(err))
		}
	} else if operateType == milvuspb.OperateUserRoleType_RemoveUserFromRole {
		err = kc.remove(ctx, k)
		if err != nil {
			log.Ctx(ctx).Error("fail to remove the user-role", zap.String("key", k), zap.Error(err))
		}
	} else {
		err = fmt.Errorf("invalid operate user role type, operate type: %d", operateType)
	}
	return err
}

func (kc *Catalog) ListRole(ctx context.Context, tenant string, entity *milvuspb.RoleEntity, includeUserInfo bool) ([]*milvuspb.RoleResult, error) {
	var results []*milvuspb.RoleResult

	roleToUsers := make(map[string][]string)
	if includeUserInfo {
		roleMappingKey := funcutil.HandleTenantForEtcdKey(RoleMappingPrefix, tenant, "")
		keys, _, err := kc.Txn.LoadWithPrefix(ctx, roleMappingKey)
		if err != nil {
			log.Ctx(ctx).Error("fail to load role mappings", zap.String("key", roleMappingKey), zap.Error(err))
			return results, err
		}

		for _, key := range keys {
			roleMappingInfos := typeutil.AfterN(key, roleMappingKey+"/", "/")
			if len(roleMappingInfos) != 2 {
				log.Ctx(ctx).Warn("invalid role mapping key", zap.String("string", key), zap.String("sub_string", roleMappingKey))
				continue
			}
			username := roleMappingInfos[0]
			roleName := roleMappingInfos[1]
			roleToUsers[roleName] = append(roleToUsers[roleName], username)
		}
	}

	appendRoleResult := func(roleName string) {
		var users []*milvuspb.UserEntity
		for _, username := range roleToUsers[roleName] {
			users = append(users, &milvuspb.UserEntity{Name: username})
		}
		results = append(results, &milvuspb.RoleResult{
			Role:  &milvuspb.RoleEntity{Name: roleName},
			Users: users,
		})
	}

	if entity == nil {
		roleKey := funcutil.HandleTenantForEtcdKey(RolePrefix, tenant, "")
		keys, _, err := kc.Txn.LoadWithPrefix(ctx, roleKey)
		if err != nil {
			log.Ctx(ctx).Error("fail to load roles", zap.String("key", roleKey), zap.Error(err))
			return results, err
		}
		for _, key := range keys {
			infoArr := typeutil.AfterN(key, roleKey+"/", "/")
			if len(infoArr) != 1 || len(infoArr[0]) == 0 {
				log.Ctx(ctx).Warn("invalid role key", zap.String("string", key), zap.String("sub_string", roleKey))
				continue
			}
			appendRoleResult(infoArr[0])
		}
	} else {
		if funcutil.IsEmptyString(entity.Name) {
			return results, errors.New("role name in the role entity is empty")
		}
		roleKey := funcutil.HandleTenantForEtcdKey(RolePrefix, tenant, entity.Name)
		_, err := kc.Txn.Load(ctx, roleKey)
		if err != nil {
			log.Ctx(ctx).Warn("fail to load a role", zap.String("key", roleKey), zap.Error(err))
			return results, err
		}
		appendRoleResult(entity.Name)
	}

	return results, nil
}

func (kc *Catalog) getRolesByUsername(ctx context.Context, tenant string, username string) ([]string, error) {
	var roles []string
	k := funcutil.HandleTenantForEtcdKey(RoleMappingPrefix, tenant, username)
	keys, _, err := kc.Txn.LoadWithPrefix(ctx, k)
	if err != nil {
		log.Ctx(ctx).Error("fail to load role mappings by the username", zap.String("key", k), zap.Error(err))
		return roles, err
	}
	for _, key := range keys {
		roleMappingInfos := typeutil.AfterN(key, k+"/", "/")
		if len(roleMappingInfos) != 1 {
			log.Ctx(ctx).Warn("invalid role mapping key", zap.String("string", key), zap.String("sub_string", k))
			continue
		}
		roles = append(roles, roleMappingInfos[0])
	}
	return roles, nil
}

// getUserResult get the user result by the username. And never return the error because the error means the user isn't added to a role.
func (kc *Catalog) getUserResult(ctx context.Context, tenant string, username string, includeRoleInfo bool) (*milvuspb.UserResult, error) {
	result := &milvuspb.UserResult{User: &milvuspb.UserEntity{Name: username}}
	if !includeRoleInfo {
		return result, nil
	}
	roleNames, err := kc.getRolesByUsername(ctx, tenant, username)
	if err != nil {
		log.Ctx(ctx).Warn("fail to get roles by the username", zap.Error(err))
		return result, err
	}
	var roles []*milvuspb.RoleEntity
	for _, roleName := range roleNames {
		roles = append(roles, &milvuspb.RoleEntity{Name: roleName})
	}
	result.Roles = roles
	return result, nil
}

func (kc *Catalog) ListUser(ctx context.Context, tenant string, entity *milvuspb.UserEntity, includeRoleInfo bool) ([]*milvuspb.UserResult, error) {
	var (
		usernames []string
		err       error
		results   []*milvuspb.UserResult
	)

	appendUserResult := func(username string) error {
		result, err := kc.getUserResult(ctx, tenant, username, includeRoleInfo)
		if err != nil {
			return err
		}
		results = append(results, result)
		return nil
	}

	if entity == nil {
		usernames, err = kc.ListCredentials(ctx)
		if err != nil {
			return results, err
		}
	} else {
		if funcutil.IsEmptyString(entity.Name) {
			return results, errors.New("username in the user entity is empty")
		}
		_, err = kc.GetCredential(ctx, entity.Name)
		if err != nil {
			return results, err
		}
		usernames = append(usernames, entity.Name)
	}
	for _, username := range usernames {
		err = appendUserResult(username)
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (kc *Catalog) AlterGrant(ctx context.Context, tenant string, entity *milvuspb.GrantEntity, operateType milvuspb.OperatePrivilegeType) error {
	var (
		privilegeName = entity.Grantor.Privilege.Name
		k             = funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, fmt.Sprintf("%s/%s/%s", entity.Role.Name, entity.Object.Name, funcutil.CombineObjectName(entity.DbName, entity.ObjectName)))
		idStr         string
		v             string
		err           error
	)

	// Compatible with logic without db
	if entity.DbName == util.DefaultDBName {
		v, err = kc.Txn.Load(ctx, funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, fmt.Sprintf("%s/%s/%s", entity.Role.Name, entity.Object.Name, entity.ObjectName)))
		if err == nil {
			idStr = v
		}
	}
	if idStr == "" {
		if v, err = kc.Txn.Load(ctx, k); err == nil {
			idStr = v
		} else {
			log.Ctx(ctx).Warn("fail to load grant privilege entity", zap.String("key", k), zap.Any("type", operateType), zap.Error(err))
			if funcutil.IsRevoke(operateType) {
				if errors.Is(err, merr.ErrIoKeyNotFound) {
					return common.NewIgnorableError(fmt.Errorf("the grant[%s] isn't existed", k))
				}
				return err
			}
			if !errors.Is(err, merr.ErrIoKeyNotFound) {
				return err
			}

			idStr = crypto.MD5(k)
			err = kc.Txn.Save(ctx, k, idStr)
			if err != nil {
				log.Ctx(ctx).Error("fail to allocate id when altering the grant", zap.Error(err))
				return err
			}
		}
	}
	k = funcutil.HandleTenantForEtcdKey(GranteeIDPrefix, tenant, fmt.Sprintf("%s/%s", idStr, privilegeName))
	_, err = kc.Txn.Load(ctx, k)
	if err != nil {
		log.Ctx(ctx).Warn("fail to load the grantee id", zap.String("key", k), zap.Error(err))
		if !errors.Is(err, merr.ErrIoKeyNotFound) {
			log.Warn("fail to load the grantee id", zap.String("key", k), zap.Error(err))
			return err
		}
		log.Ctx(ctx).Debug("not found the grantee id", zap.String("key", k))
		if funcutil.IsRevoke(operateType) {
			return common.NewIgnorableError(fmt.Errorf("the grantee-id[%s] isn't existed", k))
		}
		if funcutil.IsGrant(operateType) {
			if err = kc.Txn.Save(ctx, k, entity.Grantor.User.Name); err != nil {
				log.Ctx(ctx).Error("fail to save the grantee id", zap.String("key", k), zap.Error(err))
			}
			return err
		}
		return nil
	}
	if funcutil.IsRevoke(operateType) {
		if err = kc.Txn.Remove(ctx, k); err != nil {
			log.Ctx(ctx).Error("fail to remove the grantee id", zap.String("key", k), zap.Error(err))
			return err
		}
		return err
	}
	return common.NewIgnorableError(fmt.Errorf("the privilege[%s] has been granted", privilegeName))
}

func (kc *Catalog) ListGrant(ctx context.Context, tenant string, entity *milvuspb.GrantEntity) ([]*milvuspb.GrantEntity, error) {
	var entities []*milvuspb.GrantEntity

	var granteeKey string
	appendGrantEntity := func(v string, object string, objectName string) error {
		dbName := ""
		dbName, objectName = funcutil.SplitObjectName(objectName)
		if dbName != entity.DbName && dbName != util.AnyWord && entity.DbName != util.AnyWord {
			return nil
		}
		granteeIDKey := funcutil.HandleTenantForEtcdKey(GranteeIDPrefix, tenant, v)
		keys, values, err := kc.Txn.LoadWithPrefix(ctx, granteeIDKey)
		if err != nil {
			log.Ctx(ctx).Error("fail to load the grantee ids", zap.String("key", granteeIDKey), zap.Error(err))
			return err
		}
		for i, key := range keys {
			granteeIDInfos := typeutil.AfterN(key, granteeIDKey+"/", "/")
			if len(granteeIDInfos) != 1 {
				log.Ctx(ctx).Warn("invalid grantee id", zap.String("string", key), zap.String("sub_string", granteeIDKey))
				continue
			}
			privilegeName := util.PrivilegeNameForAPI(granteeIDInfos[0])
			if granteeIDInfos[0] == util.AnyWord {
				privilegeName = util.AnyWord
			}
			entities = append(entities, &milvuspb.GrantEntity{
				Role:       &milvuspb.RoleEntity{Name: entity.Role.Name},
				Object:     &milvuspb.ObjectEntity{Name: object},
				ObjectName: objectName,
				DbName:     dbName,
				Grantor: &milvuspb.GrantorEntity{
					User:      &milvuspb.UserEntity{Name: values[i]},
					Privilege: &milvuspb.PrivilegeEntity{Name: privilegeName},
				},
			})
		}
		return nil
	}

	if !funcutil.IsEmptyString(entity.ObjectName) && entity.Object != nil && !funcutil.IsEmptyString(entity.Object.Name) {
		if entity.DbName == util.DefaultDBName {
			granteeKey = funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, fmt.Sprintf("%s/%s/%s", entity.Role.Name, entity.Object.Name, entity.ObjectName))
			v, err := kc.Txn.Load(ctx, granteeKey)
			if err == nil {
				err = appendGrantEntity(v, entity.Object.Name, entity.ObjectName)
				if err == nil {
					return entities, nil
				}
			}
		}

		if entity.DbName != util.AnyWord {
			granteeKey = funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, fmt.Sprintf("%s/%s/%s", entity.Role.Name, entity.Object.Name, funcutil.CombineObjectName(util.AnyWord, entity.ObjectName)))
			v, err := kc.Txn.Load(ctx, granteeKey)
			if err == nil {
				_ = appendGrantEntity(v, entity.Object.Name, funcutil.CombineObjectName(util.AnyWord, entity.ObjectName))
			}
		}

		granteeKey = funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, fmt.Sprintf("%s/%s/%s", entity.Role.Name, entity.Object.Name, funcutil.CombineObjectName(entity.DbName, entity.ObjectName)))
		v, err := kc.Txn.Load(ctx, granteeKey)
		if err != nil {
			log.Ctx(ctx).Error("fail to load the grant privilege entity", zap.String("key", granteeKey), zap.Error(err))
			return entities, err
		}
		err = appendGrantEntity(v, entity.Object.Name, funcutil.CombineObjectName(entity.DbName, entity.ObjectName))
		if err != nil {
			return entities, err
		}
	} else {
		granteeKey = funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, entity.Role.Name)
		keys, values, err := kc.Txn.LoadWithPrefix(ctx, granteeKey)
		if err != nil {
			log.Ctx(ctx).Error("fail to load grant privilege entities", zap.String("key", granteeKey), zap.Error(err))
			return entities, err
		}
		for i, key := range keys {
			grantInfos := typeutil.AfterN(key, granteeKey+"/", "/")
			if len(grantInfos) != 2 {
				log.Ctx(ctx).Warn("invalid grantee key", zap.String("string", key), zap.String("sub_string", granteeKey))
				continue
			}
			err = appendGrantEntity(values[i], grantInfos[0], grantInfos[1])
			if err != nil {
				return entities, err
			}
		}
	}

	return entities, nil
}

func (kc *Catalog) DeleteGrant(ctx context.Context, tenant string, role *milvuspb.RoleEntity) error {
	var (
		k          = funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, role.Name+"/")
		err        error
		removeKeys []string
	)

	removeKeys = append(removeKeys, k)

	// the values are the grantee id list
	_, values, err := kc.Txn.LoadWithPrefix(ctx, k)
	if err != nil {
		log.Ctx(ctx).Warn("fail to load grant privilege entities", zap.String("key", k), zap.Error(err))
		return err
	}
	for _, v := range values {
		granteeIDKey := funcutil.HandleTenantForEtcdKey(GranteeIDPrefix, tenant, v+"/")
		removeKeys = append(removeKeys, granteeIDKey)
	}

	if err = kc.Txn.MultiSaveAndRemoveWithPrefix(ctx, nil, removeKeys); err != nil {
		log.Ctx(ctx).Error("fail to remove with the prefix", zap.String("key", k), zap.Error(err))
	}
	return err
}

func (kc *Catalog) ListPolicy(ctx context.Context, tenant string) ([]*milvuspb.GrantEntity, error) {
	var grants []*milvuspb.GrantEntity
	granteeKey := funcutil.HandleTenantForEtcdKey(GranteePrefix, tenant, "")
	keys, values, err := kc.Txn.LoadWithPrefix(ctx, granteeKey)
	if err != nil {
		log.Ctx(ctx).Error("fail to load all grant privilege entities", zap.String("key", granteeKey), zap.Error(err))
		return []*milvuspb.GrantEntity{}, err
	}

	for i, key := range keys {
		grantInfos := typeutil.AfterN(key, granteeKey+"/", "/")
		if len(grantInfos) != 3 {
			log.Ctx(ctx).Warn("invalid grantee key", zap.String("string", key), zap.String("sub_string", granteeKey))
			continue
		}
		granteeIDKey := funcutil.HandleTenantForEtcdKey(GranteeIDPrefix, tenant, values[i])
		idKeys, _, err := kc.Txn.LoadWithPrefix(ctx, granteeIDKey)
		if err != nil {
			log.Ctx(ctx).Error("fail to load the grantee ids", zap.String("key", granteeIDKey), zap.Error(err))
			return []*milvuspb.GrantEntity{}, err
		}
		for _, idKey := range idKeys {
			granteeIDInfos := typeutil.AfterN(idKey, granteeIDKey+"/", "/")
			if len(granteeIDInfos) != 1 {
				log.Ctx(ctx).Warn("invalid grantee id", zap.String("string", idKey), zap.String("sub_string", granteeIDKey))
				continue
			}
			dbName, objectName := funcutil.SplitObjectName(grantInfos[2])

			var privilegeName string
			if granteeIDInfos[0] == util.AnyWord {
				privilegeName = util.AnyWord
			} else {
				privilegeName = util.PrivilegeNameForAPI(granteeIDInfos[0])
			}
			grants = append(grants, &milvuspb.GrantEntity{
				Role:       &milvuspb.RoleEntity{Name: grantInfos[0]},
				Object:     &milvuspb.ObjectEntity{Name: grantInfos[1]},
				ObjectName: objectName,
				DbName:     dbName,
				Grantor: &milvuspb.GrantorEntity{
					Privilege: &milvuspb.PrivilegeEntity{Name: privilegeName},
				},
			})
		}
	}
	return grants, nil
}

func (kc *Catalog) ListUserRole(ctx context.Context, tenant string) ([]string, error) {
	var userRoles []string
	k := funcutil.HandleTenantForEtcdKey(RoleMappingPrefix, tenant, "")
	keys, _, err := kc.Txn.LoadWithPrefix(ctx, k)
	if err != nil {
		log.Ctx(ctx).Error("fail to load all user-role mappings", zap.String("key", k), zap.Error(err))
		return []string{}, err
	}

	for _, key := range keys {
		userRolesInfos := typeutil.AfterN(key, k+"/", "/")
		if len(userRolesInfos) != 2 {
			log.Ctx(ctx).Warn("invalid user-role key", zap.String("string", key), zap.String("sub_string", k))
			continue
		}
		userRoles = append(userRoles, funcutil.EncodeUserRoleCache(userRolesInfos[0], userRolesInfos[1]))
	}
	return userRoles, nil
}

func (kc *Catalog) BackupRBAC(ctx context.Context, tenant string) (*milvuspb.RBACMeta, error) {
	users, err := kc.ListUser(ctx, tenant, nil, true)
	if err != nil {
		return nil, err
	}

	credentials, err := kc.ListCredentialsWithPasswd(ctx)
	if err != nil {
		return nil, err
	}

	userInfos := lo.FilterMap(users, func(entity *milvuspb.UserResult, _ int) (*milvuspb.UserInfo, bool) {
		userName := entity.GetUser().GetName()
		if userName == util.UserRoot {
			return nil, false
		}
		return &milvuspb.UserInfo{
			User:     userName,
			Password: credentials[userName],
			Roles:    entity.GetRoles(),
		}, true
	})

	roles, err := kc.ListRole(ctx, tenant, nil, false)
	if err != nil {
		return nil, err
	}

	roleEntity := lo.FilterMap(roles, func(entity *milvuspb.RoleResult, _ int) (*milvuspb.RoleEntity, bool) {
		roleName := entity.GetRole().GetName()
		if roleName == util.RoleAdmin || roleName == util.RolePublic {
			return nil, false
		}

		return entity.GetRole(), true
	})

	grantsEntity := make([]*milvuspb.GrantEntity, 0)
	for _, role := range roleEntity {
		grants, err := kc.ListGrant(ctx, tenant, &milvuspb.GrantEntity{
			Role:   role,
			DbName: util.AnyWord,
		})
		if err != nil {
			return nil, err
		}
		grantsEntity = append(grantsEntity, grants...)
	}

	privGroups, err := kc.ListPrivilegeGroups(ctx)
	if err != nil {
		return nil, err
	}

	return &milvuspb.RBACMeta{
		Users:           userInfos,
		Roles:           roleEntity,
		Grants:          grantsEntity,
		PrivilegeGroups: privGroups,
	}, nil
}

func (kc *Catalog) RestoreRBAC(ctx context.Context, tenant string, meta *milvuspb.RBACMeta) error {
	var err error
	needRollbackUser := make([]*milvuspb.UserInfo, 0)
	needRollbackRole := make([]*milvuspb.RoleEntity, 0)
	needRollbackGrants := make([]*milvuspb.GrantEntity, 0)
	needRollbackPrivilegeGroups := make([]*milvuspb.PrivilegeGroupInfo, 0)
	defer func() {
		if err != nil {
			log.Ctx(ctx).Warn("failed to restore rbac, try to rollback", zap.Error(err))
			// roll back role
			for _, role := range needRollbackRole {
				err = kc.DropRole(ctx, tenant, role.GetName())
				if err != nil {
					log.Ctx(ctx).Warn("failed to rollback roles after restore failed", zap.Error(err))
				}
			}

			// roll back grant
			for _, grant := range needRollbackGrants {
				err = kc.AlterGrant(ctx, tenant, grant, milvuspb.OperatePrivilegeType_Revoke)
				if err != nil {
					log.Ctx(ctx).Warn("failed to rollback grants after restore failed", zap.Error(err))
				}
			}

			for _, user := range needRollbackUser {
				// roll back user
				err = kc.DropCredential(ctx, user.GetUser())
				if err != nil {
					log.Ctx(ctx).Warn("failed to rollback users after restore failed", zap.Error(err))
				}
			}

			// roll back privilege group
			for _, group := range needRollbackPrivilegeGroups {
				err = kc.DropPrivilegeGroup(ctx, group.GetGroupName())
				if err != nil {
					log.Ctx(ctx).Warn("failed to rollback privilege groups after restore failed", zap.Error(err))
				}
			}
		}
	}()

	// restore role
	existRoles, err := kc.ListRole(ctx, tenant, nil, false)
	if err != nil {
		return err
	}
	existRoleMap := lo.SliceToMap(existRoles, func(entity *milvuspb.RoleResult) (string, struct{}) { return entity.GetRole().GetName(), struct{}{} })
	for _, role := range meta.GetRoles() {
		if _, ok := existRoleMap[role.GetName()]; ok {
			log.Ctx(ctx).Warn("failed to restore, role already exists", zap.String("role", role.GetName()))
			err = errors.Newf("role [%s] already exists", role.GetName())
			return err
		}
		err = kc.CreateRole(ctx, tenant, role)
		if err != nil {
			return err
		}
		needRollbackRole = append(needRollbackRole, role)
	}

	// restore privilege group
	existPrivGroups, err := kc.ListPrivilegeGroups(ctx)
	if err != nil {
		return err
	}
	existPrivGroupMap := lo.SliceToMap(existPrivGroups, func(entity *milvuspb.PrivilegeGroupInfo) (string, struct{}) { return entity.GetGroupName(), struct{}{} })
	for _, group := range meta.GetPrivilegeGroups() {
		if _, ok := existPrivGroupMap[group.GetGroupName()]; ok {
			log.Ctx(ctx).Warn("failed to restore, privilege group already exists", zap.String("group", group.GetGroupName()))
			err = errors.Newf("privilege group [%s] already exists", group.GetGroupName())
			return err
		}
		err = kc.SavePrivilegeGroup(ctx, group)
		if err != nil {
			return err
		}
		needRollbackPrivilegeGroups = append(needRollbackPrivilegeGroups, group)
	}

	// restore grant, list latest privilege group first
	existPrivGroups, err = kc.ListPrivilegeGroups(ctx)
	if err != nil {
		return err
	}
	existPrivGroupMap = lo.SliceToMap(existPrivGroups, func(entity *milvuspb.PrivilegeGroupInfo) (string, struct{}) { return entity.GetGroupName(), struct{}{} })
	for _, grant := range meta.GetGrants() {
		privName := grant.GetGrantor().GetPrivilege().GetName()
		if util.IsPrivilegeNameDefined(privName) {
			grant.Grantor.Privilege.Name = util.PrivilegeNameForMetastore(privName)
		} else if _, ok := existPrivGroupMap[privName]; ok {
			grant.Grantor.Privilege.Name = util.PrivilegeGroupNameForMetastore(privName)
		} else {
			log.Ctx(ctx).Warn("failed to restore, privilege group does not exist", zap.String("group", privName))
			err = errors.Newf("privilege group [%s] does not exist", privName)
			return err
		}
		err = kc.AlterGrant(ctx, tenant, grant, milvuspb.OperatePrivilegeType_Grant)
		if err != nil {
			return err
		}
		needRollbackGrants = append(needRollbackGrants, grant)
	}

	// need rollback user
	existUser, err := kc.ListUser(ctx, tenant, nil, false)
	if err != nil {
		return err
	}
	existUserMap := lo.SliceToMap(existUser, func(entity *milvuspb.UserResult) (string, struct{}) { return entity.GetUser().GetName(), struct{}{} })
	for _, user := range meta.GetUsers() {
		if _, ok := existUserMap[user.GetUser()]; ok {
			log.Ctx(ctx).Info("failed to restore, user already exists", zap.String("user", user.GetUser()))
			err = errors.Newf("user [%s] already exists", user.GetUser())
			return err
		}
		// restore user
		err = kc.CreateCredential(ctx, &model.Credential{
			Username:          user.GetUser(),
			EncryptedPassword: user.GetPassword(),
		})
		if err != nil {
			return err
		}
		needRollbackUser = append(needRollbackUser, user)

		// restore user role mapping
		entity := &milvuspb.UserEntity{
			Name: user.GetUser(),
		}
		for _, role := range user.GetRoles() {
			err = kc.AlterUserRole(ctx, tenant, entity, role, milvuspb.OperateUserRoleType_AddUserToRole)
			if err != nil {
				return err
			}
		}
	}

	return err
}

func (kc *Catalog) GetPrivilegeGroup(ctx context.Context, groupName string) (*milvuspb.PrivilegeGroupInfo, error) {
	k := BuildPrivilegeGroupkey(groupName)
	val, err := kc.Txn.Load(ctx, k)
	if err != nil {
		if errors.Is(err, merr.ErrIoKeyNotFound) {
			return nil, fmt.Errorf("privilege group [%s] does not exist", groupName)
		}
		log.Ctx(ctx).Error("failed to load privilege group", zap.String("group", groupName), zap.Error(err))
		return nil, err
	}
	privGroupInfo := &milvuspb.PrivilegeGroupInfo{}
	err = proto.Unmarshal([]byte(val), privGroupInfo)
	if err != nil {
		log.Ctx(ctx).Error("failed to unmarshal privilege group info", zap.Error(err))
		return nil, err
	}
	return privGroupInfo, nil
}

func (kc *Catalog) DropPrivilegeGroup(ctx context.Context, groupName string) error {
	k := BuildPrivilegeGroupkey(groupName)
	err := kc.Txn.Remove(ctx, k)
	if err != nil {
		log.Ctx(ctx).Warn("fail to drop privilege group", zap.String("key", k), zap.Error(err))
		return err
	}
	return nil
}

func (kc *Catalog) SavePrivilegeGroup(ctx context.Context, data *milvuspb.PrivilegeGroupInfo) error {
	k := BuildPrivilegeGroupkey(data.GroupName)
	groupInfo := &milvuspb.PrivilegeGroupInfo{
		GroupName:  data.GroupName,
		Privileges: lo.Uniq(data.Privileges),
	}
	v, err := proto.Marshal(groupInfo)
	if err != nil {
		log.Ctx(ctx).Error("failed to marshal privilege group info", zap.Error(err))
		return err
	}
	if err = kc.Txn.Save(ctx, k, string(v)); err != nil {
		log.Ctx(ctx).Warn("fail to put privilege group", zap.String("key", k), zap.Error(err))
		return err
	}
	return nil
}

func (kc *Catalog) ListPrivilegeGroups(ctx context.Context) ([]*milvuspb.PrivilegeGroupInfo, error) {
	_, vals, err := kc.Txn.LoadWithPrefix(ctx, PrivilegeGroupPrefix)
	if err != nil {
		log.Ctx(ctx).Error("failed to list privilege groups", zap.String("prefix", PrivilegeGroupPrefix), zap.Error(err))
		return nil, err
	}
	privGroups := make([]*milvuspb.PrivilegeGroupInfo, 0, len(vals))
	for _, val := range vals {
		privGroupInfo := &milvuspb.PrivilegeGroupInfo{}
		err = proto.Unmarshal([]byte(val), privGroupInfo)
		if err != nil {
			log.Ctx(ctx).Error("failed to unmarshal privilege group info", zap.Error(err))
			return nil, err
		}
		privGroups = append(privGroups, privGroupInfo)
	}
	return privGroups, nil
}

func (kc *Catalog) Close() {
	// do nothing
}

func isDefaultDB(dbID int64) bool {
	if dbID == util.DefaultDBID || dbID == util.NonDBID {
		return true
	}
	return false
}
